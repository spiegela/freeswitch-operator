/*
 * Silero VAD on ONNX Runtime: one shared model, and a small state per audio stream.
 *
 * The model (github.com/snakers4/silero-vad, MIT) takes 256 samples at 8 kHz or 512 at 16 kHz per call, prefixed by
 * the previous call's last 32 or 64 samples, plus its recurrent state, and returns the window's speech probability
 * and the next state. This file knows nothing of FreeSWITCH, so it can be built and tested on its own.
 *
 * Copyright 2026 The freeswitch-operator Authors. Licensed under the Apache License, Version 2.0.
 */
#ifndef SPEECH_DETECT_SILERO_H
#define SPEECH_DETECT_SILERO_H

#include <stddef.h>

typedef struct silero_model silero_model_t;
typedef struct silero_stream silero_stream_t;

/* Loads the model at path. Returns NULL and fills err on failure. Safe to share between threads. */
silero_model_t *silero_model_load(const char *path, char *err, size_t errlen);
void silero_model_free(silero_model_t *model);

/* Whether the model takes audio at this rate (8000 or 16000). */
int silero_rate_supported(int rate);

/* The samples one inference takes at this rate: 256 at 8 kHz, 512 at 16 kHz. */
size_t silero_window(int rate);

/* A stream's state at rate (8000 or 16000); NULL if out of memory or the rate is unsupported. */
silero_stream_t *silero_stream_new(silero_model_t *model, int rate);
void silero_stream_free(silero_stream_t *stream);
void silero_stream_reset(silero_stream_t *stream);

/* Scores one window of silero_window(rate) samples in [-1, 1]. Returns 0 and sets *prob, or -1 and fills err. */
int silero_stream_infer(silero_stream_t *stream, const float *window, float *prob, char *err, size_t errlen);

#endif
