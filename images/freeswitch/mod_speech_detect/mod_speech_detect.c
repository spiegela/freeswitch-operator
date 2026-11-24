/*
 * mod_speech_detect: tells, on a channel's incoming audio, when someone starts and stops talking.
 *
 *   uuid_speech_detect <uuid> start <name> silero [threshold=0.5] [min_speech_ms=250] [min_silence_ms=100]
 *   uuid_speech_detect <uuid> start <name> webrtc [mode=3] [voice_ms=100] [silence_ms=300]
 *   uuid_speech_detect <uuid> stop <name>
 *
 * A detector is a media bug on what the channel reads (what its far end sends), named so that one channel can run
 * several and a listener can tell them apart; it runs until stopped or the channel hangs up. On each change it fires
 * CUSTOM speech_detect::change with the channel's data and:
 *
 *   Speech-Event        started or stopped
 *   Speech-Detector     the name it was started with
 *   Speech-Engine       silero or webrtc
 *   Speech-At           when the speech started or stopped, in Unix milliseconds: dated back from the detection by the
 *                       speech or silence it took to decide
 *   Speech-Probability  silero only: the deciding window's speech probability
 *
 * silero is Silero VAD (github.com/snakers4/silero-vad, MIT), a small neural network, on ONNX Runtime, scoring 32 ms
 * windows. A start takes min_speech_ms of windows at or above threshold; a stop takes min_silence_ms of windows below
 * threshold - 0.15 (Silero's own hysteresis). It reads 8 or 16 kHz audio as it comes, and resamples any other rate to
 * 16 kHz. The model is loaded once, from speech_detect.conf's silero-model (default
 * /usr/local/share/speech_detect/silero_vad.onnx).
 *
 * webrtc is FreeSWITCH's own detector (switch_vad), WebRTC's GMM detector when FreeSWITCH is built with libfvad:
 * mode 0 to 3, from the most to the least willing to call audio speech; a start takes voice_ms of speech, a stop
 * silence_ms of silence. It gives no probability.
 *
 * Copyright 2026 The freeswitch-operator Authors. Licensed under the Apache License, Version 2.0.
 */
#include <switch.h>

#include "silero.h"

SWITCH_MODULE_LOAD_FUNCTION(mod_speech_detect_load);
SWITCH_MODULE_SHUTDOWN_FUNCTION(mod_speech_detect_shutdown);
SWITCH_MODULE_DEFINITION(mod_speech_detect, mod_speech_detect_load, mod_speech_detect_shutdown, NULL);

#define EVENT_CHANGE "speech_detect::change"
#define PRIVATE_PREFIX "speech_detect:"
#define DEFAULT_MODEL "/usr/local/share/speech_detect/silero_vad.onnx"
#define USAGE \
	"<uuid> start <name> silero [threshold=0.5] [min_speech_ms=250] [min_silence_ms=100]" \
	" | <uuid> start <name> webrtc [mode=3] [voice_ms=100] [silence_ms=300]" \
	" | <uuid> stop <name>"
#define MAX_ARGS 16

static struct {
	silero_model_t *silero;
	char model_path[512];
	char model_error[512];
} globals;

typedef enum { ENGINE_SILERO, ENGINE_WEBRTC } engine_t;

typedef struct {
	switch_core_session_t *session;
	char name[64];
	char key[96]; /* the channel private holding the bug */
	engine_t engine;
	uint32_t rate; /* of the audio as read; 0 until the first frame */
	int talking;

	/* silero */
	float threshold, neg_threshold;
	uint32_t min_speech_ms, min_silence_ms;
	silero_stream_t *stream;
	switch_audio_resampler_t *resampler; /* when the read rate is neither 8 nor 16 kHz */
	uint32_t model_rate;
	float *window;
	size_t window_len, window_fill;
	uint64_t samples;     /* model-rate samples scored so far */
	int64_t speech_from;  /* where the current run of speech windows began, or -1 */
	int64_t silence_from; /* where the current run of silence windows began, or -1 */

	/* webrtc */
	int mode, voice_ms, silence_ms;
	switch_vad_t *vad;
} detector_t;

static void fire(detector_t *d, int started, switch_time_t at_ms, float prob, int has_prob)
{
	switch_event_t *event;

	if (switch_event_create_subclass(&event, SWITCH_EVENT_CUSTOM, EVENT_CHANGE) != SWITCH_STATUS_SUCCESS) return;
	switch_channel_event_set_data(switch_core_session_get_channel(d->session), event);
	switch_event_add_header_string(event, SWITCH_STACK_BOTTOM, "Speech-Event", started ? "started" : "stopped");
	switch_event_add_header_string(event, SWITCH_STACK_BOTTOM, "Speech-Detector", d->name);
	switch_event_add_header_string(event, SWITCH_STACK_BOTTOM, "Speech-Engine", d->engine == ENGINE_SILERO ? "silero" : "webrtc");
	switch_event_add_header(event, SWITCH_STACK_BOTTOM, "Speech-At", "%" SWITCH_TIME_T_FMT, at_ms);
	if (has_prob) switch_event_add_header(event, SWITCH_STACK_BOTTOM, "Speech-Probability", "%.3f", prob);
	switch_event_fire(&event);
	switch_log_printf(SWITCH_CHANNEL_SESSION_LOG(d->session), SWITCH_LOG_DEBUG, "speech_detect %s: %s\n", d->name,
					  started ? "started" : "stopped");
}

/* Unix milliseconds, less the given audio's duration. */
static switch_time_t ago_ms(uint64_t samples, uint32_t rate)
{
	return switch_micro_time_now() / 1000 - (switch_time_t) (samples * 1000 / rate);
}

/* Scores one full window and moves the start/stop state on. */
static void silero_window_done(detector_t *d)
{
	char err[256];
	float prob;
	int64_t at = (int64_t) d->samples, end = at + (int64_t) d->window_len;

	d->samples = (uint64_t) end;
	if (silero_stream_infer(d->stream, d->window, &prob, err, sizeof(err))) {
		switch_log_printf(SWITCH_CHANNEL_SESSION_LOG(d->session), SWITCH_LOG_ERROR, "speech_detect %s: %s\n", d->name, err);
		return;
	}
	if (!d->talking) {
		if (prob < d->threshold) {
			d->speech_from = -1;
			return;
		}
		if (d->speech_from < 0) d->speech_from = at;
		if ((uint64_t) (end - d->speech_from) * 1000 >= (uint64_t) d->min_speech_ms * d->model_rate) {
			d->talking = 1;
			d->silence_from = -1;
			fire(d, 1, ago_ms((uint64_t) (end - d->speech_from), d->model_rate), prob, 1);
		}
		return;
	}
	if (prob >= d->threshold) {
		d->silence_from = -1;
		return;
	}
	if (prob >= d->neg_threshold) return; /* neither: a silence run in progress goes on */
	if (d->silence_from < 0) d->silence_from = at;
	if ((uint64_t) (end - d->silence_from) * 1000 >= (uint64_t) d->min_silence_ms * d->model_rate) {
		d->talking = 0;
		d->speech_from = -1;
		fire(d, 0, ago_ms((uint64_t) (end - d->silence_from), d->model_rate), prob, 1);
	}
}

static void silero_feed(detector_t *d, const int16_t *data, uint32_t samples)
{
	for (uint32_t i = 0; i < samples; i++) {
		d->window[d->window_fill++] = (float) data[i] / 32768.0f;
		if (d->window_fill == d->window_len) {
			d->window_fill = 0;
			silero_window_done(d);
		}
	}
}

static void detector_release(detector_t *d)
{
	if (d->stream) silero_stream_free(d->stream);
	if (d->resampler) switch_resample_destroy(&d->resampler);
	if (d->vad) switch_vad_destroy(&d->vad);
	free(d->window);
	d->stream = NULL;
	d->window = NULL;
}

/* Sets the detector up for audio read at rate: on the first frame, and again if the rate changes mid-call. */
static switch_status_t detector_configure(detector_t *d, uint32_t rate)
{
	detector_release(d);
	d->rate = rate;
	d->talking = 0;
	if (d->engine == ENGINE_WEBRTC) {
		if (!(d->vad = switch_vad_init((int) rate, 1))) return SWITCH_STATUS_FALSE;
		switch_vad_set_mode(d->vad, d->mode);
		switch_vad_set_param(d->vad, "voice_ms", d->voice_ms);
		switch_vad_set_param(d->vad, "silence_ms", d->silence_ms);
		return SWITCH_STATUS_SUCCESS;
	}
	d->model_rate = silero_rate_supported((int) rate) ? rate : 16000;
	if (d->model_rate != rate &&
		switch_resample_create(&d->resampler, rate, d->model_rate, rate / 10, SWITCH_RESAMPLE_QUALITY, 1) != SWITCH_STATUS_SUCCESS)
		return SWITCH_STATUS_FALSE;
	d->window_len = silero_window((int) d->model_rate);
	d->window_fill = 0;
	d->samples = 0;
	d->speech_from = d->silence_from = -1;
	if (!(d->window = calloc(d->window_len, sizeof(float))) ||
		!(d->stream = silero_stream_new(globals.silero, (int) d->model_rate)))
		return SWITCH_STATUS_FALSE;
	return SWITCH_STATUS_SUCCESS;
}

static void detector_process(detector_t *d, switch_frame_t *frame)
{
	uint32_t samples = frame->datalen / sizeof(int16_t);
	uint32_t rate = frame->rate;

	if (!rate) {
		switch_codec_implementation_t impl = {0};
		switch_core_session_get_read_impl(d->session, &impl);
		rate = impl.actual_samples_per_second;
	}
	if (!rate || !samples) return;
	if (rate != d->rate && detector_configure(d, rate) != SWITCH_STATUS_SUCCESS) {
		switch_log_printf(SWITCH_CHANNEL_SESSION_LOG(d->session), SWITCH_LOG_ERROR,
						  "speech_detect %s: cannot detect on %u Hz audio\n", d->name, rate);
		detector_release(d);
		return;
	}
	if (d->engine == ENGINE_WEBRTC) {
		if (!d->vad) return;
		switch (switch_vad_process(d->vad, (int16_t *) frame->data, samples)) {
		case SWITCH_VAD_STATE_START_TALKING:
			d->talking = 1;
			fire(d, 1, ago_ms((uint64_t) d->voice_ms * rate / 1000, rate), 0, 0);
			break;
		case SWITCH_VAD_STATE_STOP_TALKING:
			d->talking = 0;
			fire(d, 0, ago_ms((uint64_t) d->silence_ms * rate / 1000, rate), 0, 0);
			break;
		default:
			break;
		}
		return;
	}
	if (!d->stream) return;
	if (d->resampler) {
		switch_resample_process(d->resampler, (int16_t *) frame->data, samples);
		silero_feed(d, d->resampler->to, d->resampler->to_len);
	} else {
		silero_feed(d, (const int16_t *) frame->data, samples);
	}
}

static switch_bool_t bug_callback(switch_media_bug_t *bug, void *user_data, switch_abc_type_t type)
{
	detector_t *d = user_data;

	switch (type) {
	case SWITCH_ABC_TYPE_READ_REPLACE: {
		/* Read, never changed: the frame goes on as it came. */
		switch_frame_t *frame = switch_core_media_bug_get_read_replace_frame(bug);
		if (frame && frame->data && !switch_test_flag(frame, SFF_CNG)) detector_process(d, frame);
		break;
	}
	case SWITCH_ABC_TYPE_CLOSE:
		switch_channel_set_private(switch_core_session_get_channel(d->session), d->key, NULL);
		detector_release(d);
		free(d);
		break;
	default:
		break;
	}
	return SWITCH_TRUE;
}

/* Parses "key=value" settings into the detector; returns the first bad one, or NULL. */
static const char *apply_settings(detector_t *d, char **argv, int argc)
{
	for (int i = 0; i < argc; i++) {
		char *key = argv[i], *val = strchr(argv[i], '=');
		char *end = NULL;
		double v;

		if (!val) return argv[i];
		*val++ = '\0';
		v = strtod(val, &end);
		if (zstr(val) || *end || v < 0) return key;
		if (d->engine == ENGINE_SILERO && !strcasecmp(key, "threshold") && v > 0 && v < 1) d->threshold = (float) v;
		else if (d->engine == ENGINE_SILERO && !strcasecmp(key, "min_speech_ms")) d->min_speech_ms = (uint32_t) v;
		else if (d->engine == ENGINE_SILERO && !strcasecmp(key, "min_silence_ms")) d->min_silence_ms = (uint32_t) v;
		else if (d->engine == ENGINE_WEBRTC && !strcasecmp(key, "mode") && v <= 3) d->mode = (int) v;
		else if (d->engine == ENGINE_WEBRTC && !strcasecmp(key, "voice_ms")) d->voice_ms = (int) v;
		else if (d->engine == ENGINE_WEBRTC && !strcasecmp(key, "silence_ms")) d->silence_ms = (int) v;
		else return key;
	}
	return NULL;
}

static void start_detector(switch_core_session_t *session, const char *name, const char *engine, char **settings,
						   int nsettings, switch_stream_handle_t *stream)
{
	switch_channel_t *channel = switch_core_session_get_channel(session);
	switch_media_bug_t *bug = NULL;
	detector_t *d;
	const char *bad;

	if (strlen(name) >= sizeof(d->name)) {
		stream->write_function(stream, "-ERR detector name too long\n");
		return;
	}
	if (!(d = calloc(1, sizeof(*d)))) {
		stream->write_function(stream, "-ERR out of memory\n");
		return;
	}
	d->session = session;
	switch_copy_string(d->name, name, sizeof(d->name));
	switch_snprintf(d->key, sizeof(d->key), "%s%s", PRIVATE_PREFIX, name);
	d->threshold = 0.5f;
	d->min_speech_ms = 250;
	d->min_silence_ms = 100;
	d->mode = 3;
	d->voice_ms = 100;
	d->silence_ms = 300;
	if (!strcasecmp(engine, "silero")) {
		d->engine = ENGINE_SILERO;
		if (!globals.silero) {
			stream->write_function(stream, "-ERR silero unavailable: %s\n", globals.model_error);
			goto fail;
		}
	} else if (!strcasecmp(engine, "webrtc")) {
		d->engine = ENGINE_WEBRTC;
	} else {
		stream->write_function(stream, "-ERR unknown engine %s (silero or webrtc)\n", engine);
		goto fail;
	}
	if ((bad = apply_settings(d, settings, nsettings))) {
		stream->write_function(stream, "-ERR bad setting %s for %s\n", bad, engine);
		goto fail;
	}
	d->neg_threshold = d->threshold > 0.16f ? d->threshold - 0.15f : 0.01f;
	if (switch_channel_get_private(channel, d->key)) {
		stream->write_function(stream, "-ERR detector %s already running\n", name);
		goto fail;
	}
	if (switch_core_media_bug_add(session, "speech_detect", name, bug_callback, d, 0, SMBF_READ_REPLACE | SMBF_NO_PAUSE, &bug) !=
		SWITCH_STATUS_SUCCESS) {
		stream->write_function(stream, "-ERR cannot add a media bug\n");
		goto fail;
	}
	/* From here the bug owns d, and frees it when it closes. */
	switch_channel_set_private(channel, d->key, bug);
	stream->write_function(stream, "+OK\n");
	return;

fail:
	free(d);
}

static void stop_detector(switch_core_session_t *session, const char *name, switch_stream_handle_t *stream)
{
	switch_channel_t *channel = switch_core_session_get_channel(session);
	char key[96];
	switch_media_bug_t *bug;

	switch_snprintf(key, sizeof(key), "%s%s", PRIVATE_PREFIX, name);
	if (!(bug = switch_channel_get_private(channel, key))) {
		stream->write_function(stream, "-ERR no detector %s\n", name);
		return;
	}
	switch_channel_set_private(channel, key, NULL);
	switch_core_media_bug_remove(session, &bug);
	stream->write_function(stream, "+OK\n");
}

SWITCH_STANDARD_API(speech_detect_api)
{
	char *mydata = NULL, *argv[MAX_ARGS] = {0};
	int argc = 0;
	switch_core_session_t *target = NULL;

	if (zstr(cmd) || !(mydata = strdup(cmd)) || (argc = switch_separate_string(mydata, ' ', argv, MAX_ARGS)) < 3) {
		stream->write_function(stream, "-USAGE: %s\n", USAGE);
		goto done;
	}
	if (!(target = switch_core_session_locate(argv[0]))) {
		stream->write_function(stream, "-ERR no such channel %s\n", argv[0]);
		goto done;
	}
	if (!strcasecmp(argv[1], "start") && argc >= 4) {
		start_detector(target, argv[2], argv[3], argv + 4, argc - 4, stream);
	} else if (!strcasecmp(argv[1], "stop") && argc == 3) {
		stop_detector(target, argv[2], stream);
	} else {
		stream->write_function(stream, "-USAGE: %s\n", USAGE);
	}

done:
	if (target) switch_core_session_rwunlock(target);
	switch_safe_free(mydata);
	return SWITCH_STATUS_SUCCESS;
}

static void load_config(void)
{
	switch_xml_t cfg, xml, settings, param;

	switch_copy_string(globals.model_path, DEFAULT_MODEL, sizeof(globals.model_path));
	if (!(xml = switch_xml_open_cfg("speech_detect.conf", &cfg, NULL))) return;
	if ((settings = switch_xml_child(cfg, "settings"))) {
		for (param = switch_xml_child(settings, "param"); param; param = param->next) {
			const char *name = switch_xml_attr_soft(param, "name"), *value = switch_xml_attr_soft(param, "value");
			if (!strcasecmp(name, "silero-model") && !zstr(value)) switch_copy_string(globals.model_path, value, sizeof(globals.model_path));
		}
	}
	switch_xml_free(xml);
}

SWITCH_MODULE_LOAD_FUNCTION(mod_speech_detect_load)
{
	switch_api_interface_t *api;

	memset(&globals, 0, sizeof(globals));
	if (switch_event_reserve_subclass(EVENT_CHANGE) != SWITCH_STATUS_SUCCESS) {
		switch_log_printf(SWITCH_CHANNEL_LOG, SWITCH_LOG_ERROR, "cannot reserve event subclass %s\n", EVENT_CHANGE);
		return SWITCH_STATUS_TERM;
	}
	load_config();
	/* Without the model, silero is refused and webrtc still works. */
	if (!(globals.silero = silero_model_load(globals.model_path, globals.model_error, sizeof(globals.model_error))))
		switch_log_printf(SWITCH_CHANNEL_LOG, SWITCH_LOG_ERROR, "speech_detect: silero unavailable: %s\n", globals.model_error);
	else
		switch_log_printf(SWITCH_CHANNEL_LOG, SWITCH_LOG_INFO, "speech_detect: silero model %s\n", globals.model_path);

	*module_interface = switch_loadable_module_create_module_interface(pool, modname);
	SWITCH_ADD_API(api, "uuid_speech_detect", "Detect speech on a channel's incoming audio", speech_detect_api, USAGE);
	switch_console_set_complete("add uuid_speech_detect ::console::list_uuid start");
	switch_console_set_complete("add uuid_speech_detect ::console::list_uuid stop");
	return SWITCH_STATUS_SUCCESS;
}

SWITCH_MODULE_SHUTDOWN_FUNCTION(mod_speech_detect_shutdown)
{
	switch_event_free_subclass(EVENT_CHANGE);
	silero_model_free(globals.silero);
	globals.silero = NULL;
	return SWITCH_STATUS_SUCCESS;
}
