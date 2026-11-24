/*
 * Silero VAD on ONNX Runtime's C API. See silero.h.
 *
 * Copyright 2026 The freeswitch-operator Authors. Licensed under the Apache License, Version 2.0.
 */
#include "silero.h"

#include <onnxruntime_c_api.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define STATE_LEN (2 * 1 * 128) /* the recurrent state: [2, batch 1, 128] */

/* The model's inputs, by name; its outputs are taken in order: the probability, then the next state. */
enum { IN_AUDIO, IN_STATE, IN_RATE, IN_COUNT };
static const char *const input_names[IN_COUNT] = {"input", "state", "sr"};

struct silero_model {
	const OrtApi *ort;
	OrtEnv *env;
	OrtSession *session;
	OrtMemoryInfo *memory;
	char *output_names[2];
};

struct silero_stream {
	silero_model_t *model;
	int64_t rate;
	size_t window, context;
	float *input; /* context, then window */
	float state[STATE_LEN];
};

/* Moves an ONNX Runtime error into err; returns -1 if there was one. */
static int failed(const OrtApi *ort, OrtStatus *status, const char *what, char *err, size_t errlen)
{
	if (!status) return 0;
	snprintf(err, errlen, "%s: %s", what, ort->GetErrorMessage(status));
	ort->ReleaseStatus(status);
	return -1;
}

/* Frees a string ONNX Runtime allocated; that fails only on a bad allocator, so its status is just released. */
static void free_name(const OrtApi *ort, OrtAllocator *alloc, char *name)
{
	OrtStatus *status = ort->AllocatorFree(alloc, name);
	if (status) ort->ReleaseStatus(status);
}

int silero_rate_supported(int rate) { return rate == 8000 || rate == 16000; }

size_t silero_window(int rate) { return rate == 16000 ? 512 : 256; }

static size_t context_of(int rate) { return rate == 16000 ? 64 : 32; }

silero_model_t *silero_model_load(const char *path, char *err, size_t errlen)
{
	const OrtApiBase *base = OrtGetApiBase();
	const OrtApi *ort = base ? base->GetApi(ORT_API_VERSION) : NULL;
	OrtSessionOptions *opts = NULL;
	OrtAllocator *alloc = NULL;
	silero_model_t *m;
	size_t n = 0;

	if (!ort) {
		snprintf(err, errlen, "ONNX Runtime %s lacks API version %d", base ? base->GetVersionString() : "?", ORT_API_VERSION);
		return NULL;
	}
	if (!(m = calloc(1, sizeof(*m)))) {
		snprintf(err, errlen, "out of memory");
		return NULL;
	}
	m->ort = ort;
	/* ONNX Runtime's Linux builds (1.32 at least) carry Microsoft's telemetry client, which CreateEnv starts and which
	 * uploads to mobile.events.data.microsoft.com. An edge must not phone home: ORT_DISABLE_TELEMETRY (undocumented;
	 * read by CreateEnv) keeps the client from starting, unless whoever runs FreeSWITCH set it otherwise, and
	 * DisableTelemetryEvents turns the events off after. The image sets the variable too. */
	setenv("ORT_DISABLE_TELEMETRY", "1", 0);
	if (failed(ort, ort->CreateEnv(ORT_LOGGING_LEVEL_WARNING, "speech_detect", &m->env), "creating the environment", err, errlen)) goto fail;
	if (failed(ort, ort->DisableTelemetryEvents(m->env), "disabling telemetry", err, errlen)) goto fail;
	if (failed(ort, ort->CreateSessionOptions(&opts), "session options", err, errlen)) goto fail;
	/* Each stream's inference runs on the media thread that calls it, one window at a time: no thread pools, and no
	 * spinning between windows. One session serves every stream; ONNX Runtime allows concurrent Run calls. */
	if (failed(ort, ort->SetIntraOpNumThreads(opts, 1), "session options", err, errlen) ||
		failed(ort, ort->SetInterOpNumThreads(opts, 1), "session options", err, errlen) ||
		failed(ort, ort->AddSessionConfigEntry(opts, "session.intra_op.allow_spinning", "0"), "session options", err, errlen) ||
		failed(ort, ort->AddSessionConfigEntry(opts, "session.inter_op.allow_spinning", "0"), "session options", err, errlen) ||
		failed(ort, ort->SetSessionGraphOptimizationLevel(opts, ORT_ENABLE_ALL), "session options", err, errlen))
		goto fail;
	if (failed(ort, ort->CreateSession(m->env, path, opts, &m->session), path, err, errlen)) goto fail;
	if (failed(ort, ort->CreateCpuMemoryInfo(OrtArenaAllocator, OrtMemTypeDefault, &m->memory), "memory info", err, errlen)) goto fail;

	/* Check the model is the one this code feeds: three inputs by these names, two outputs. */
	if (failed(ort, ort->GetAllocatorWithDefaultOptions(&alloc), "allocator", err, errlen)) goto fail;
	if (failed(ort, ort->SessionGetInputCount(m->session, &n), "inputs", err, errlen)) goto fail;
	for (int want = 0; want < IN_COUNT; want++) {
		int found = 0;
		for (size_t i = 0; i < n && !found; i++) {
			char *name = NULL;
			if (failed(ort, ort->SessionGetInputName(m->session, i, alloc, &name), "inputs", err, errlen)) goto fail;
			found = strcmp(name, input_names[want]) == 0;
			free_name(ort, alloc, name);
		}
		if (!found) {
			snprintf(err, errlen, "%s: no input \"%s\"; not a Silero VAD v5 or later model", path, input_names[want]);
			goto fail;
		}
	}
	if (failed(ort, ort->SessionGetOutputCount(m->session, &n), "outputs", err, errlen)) goto fail;
	if (n != 2) {
		snprintf(err, errlen, "%s: %zu outputs, not 2; not a Silero VAD v5 or later model", path, n);
		goto fail;
	}
	for (size_t i = 0; i < 2; i++) {
		char *name = NULL;
		if (failed(ort, ort->SessionGetOutputName(m->session, i, alloc, &name), "outputs", err, errlen)) goto fail;
		m->output_names[i] = strdup(name);
		free_name(ort, alloc, name);
		if (!m->output_names[i]) {
			snprintf(err, errlen, "out of memory");
			goto fail;
		}
	}
	ort->ReleaseSessionOptions(opts);
	return m;

fail:
	if (opts) ort->ReleaseSessionOptions(opts);
	silero_model_free(m);
	return NULL;
}

void silero_model_free(silero_model_t *m)
{
	if (!m) return;
	if (m->memory) m->ort->ReleaseMemoryInfo(m->memory);
	if (m->session) m->ort->ReleaseSession(m->session);
	if (m->env) m->ort->ReleaseEnv(m->env);
	free(m->output_names[0]);
	free(m->output_names[1]);
	free(m);
}

silero_stream_t *silero_stream_new(silero_model_t *model, int rate)
{
	silero_stream_t *s;

	if (!model || !silero_rate_supported(rate) || !(s = calloc(1, sizeof(*s)))) return NULL;
	s->model = model;
	s->rate = rate;
	s->window = silero_window(rate);
	s->context = context_of(rate);
	if (!(s->input = calloc(s->context + s->window, sizeof(float)))) {
		free(s);
		return NULL;
	}
	return s;
}

void silero_stream_reset(silero_stream_t *s)
{
	memset(s->input, 0, (s->context + s->window) * sizeof(float));
	memset(s->state, 0, sizeof(s->state));
}

void silero_stream_free(silero_stream_t *s)
{
	if (!s) return;
	free(s->input);
	free(s);
}

int silero_stream_infer(silero_stream_t *s, const float *window, float *prob, char *err, size_t errlen)
{
	const OrtApi *ort = s->model->ort;
	int64_t audio_shape[2] = {1, (int64_t) (s->context + s->window)};
	int64_t state_shape[3] = {2, 1, 128};
	int64_t scalar_shape[1] = {0};
	OrtValue *in[IN_COUNT] = {NULL, NULL, NULL}, *out[2] = {NULL, NULL};
	float *p = NULL, *next = NULL;
	int rc = -1;

	/* The window goes after the context the last call left at the front. */
	memcpy(s->input + s->context, window, s->window * sizeof(float));

	if (failed(ort, ort->CreateTensorWithDataAsOrtValue(s->model->memory, s->input, (s->context + s->window) * sizeof(float),
					audio_shape, 2, ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT, &in[IN_AUDIO]), "audio tensor", err, errlen) ||
		failed(ort, ort->CreateTensorWithDataAsOrtValue(s->model->memory, s->state, sizeof(s->state),
					state_shape, 3, ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT, &in[IN_STATE]), "state tensor", err, errlen) ||
		failed(ort, ort->CreateTensorWithDataAsOrtValue(s->model->memory, &s->rate, sizeof(s->rate),
					scalar_shape, 0, ONNX_TENSOR_ELEMENT_DATA_TYPE_INT64, &in[IN_RATE]), "rate tensor", err, errlen))
		goto done;
	if (failed(ort, ort->Run(s->model->session, NULL, input_names, (const OrtValue *const *) in, IN_COUNT,
					(const char *const *) s->model->output_names, 2, out), "inference", err, errlen))
		goto done;
	if (failed(ort, ort->GetTensorMutableData(out[0], (void **) &p), "output", err, errlen) ||
		failed(ort, ort->GetTensorMutableData(out[1], (void **) &next), "next state", err, errlen))
		goto done;
	*prob = p[0];
	memcpy(s->state, next, sizeof(s->state));
	/* The next call's context: this window's last samples. */
	memmove(s->input, s->input + s->window, s->context * sizeof(float));
	rc = 0;

done:
	for (int i = 0; i < IN_COUNT; i++) if (in[i]) ort->ReleaseValue(in[i]);
	for (int i = 0; i < 2; i++) if (out[i]) ort->ReleaseValue(out[i]);
	return rc;
}
