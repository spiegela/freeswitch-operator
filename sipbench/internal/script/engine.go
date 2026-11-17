package script

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"

	"git.instruments.com/ci/freeswitch-operator/sipbench/internal/tone"
)

// ToneAmplitude is the peak level of generated tones, about -12 dBFS.
const ToneAmplitude = 8000

// Engine runs media steps on an answered (or early-media) call and listens to everything received: tones at the
// frequencies it was given, and RFC 2833 keypad tones.
type Engine struct {
	codec    media.Codec
	detector *tone.Detector

	writeMu sync.Mutex
	enc     io.Writer
	dtmf    *diago.DTMFWriter

	digitsMu sync.Mutex
	digits   []rune

	echoUntil atomic.Int64
	frames    atomic.Int64
	done      chan struct{}

	// The pacer sends a frame every 20 ms, as a phone does: the current tone if one is playing, else silence.
	// A continuous stream keeps the RTP clock moving, which RFC 2833 receivers rely on to tell events apart.
	toneMu   sync.Mutex
	toneGen  *tone.Generator
	toneEnd  time.Time
	writeErr atomic.Pointer[error]
}

// Start begins listening on med. freqs are the tone frequencies to detect.
func Start(med *diago.DialogMedia, freqs []float64) (*Engine, error) {
	ms := med.MediaSession()
	if ms == nil {
		return nil, fmt.Errorf("call has no media session")
	}
	codec := media.CodecAudioFromSession(ms)
	e := &Engine{
		codec:    codec,
		detector: tone.NewDetector(freqs),
		done:     make(chan struct{}),
	}

	// Writer: prefer the DTMF-capable writer so tones and keypad events share one RTP stream.
	var w io.Writer
	if dw, err := med.AudioWriterDTMF(); err == nil {
		e.dtmf = dw
		w = dw
	} else {
		aw, err := med.AudioWriter()
		if err != nil {
			return nil, fmt.Errorf("audio writer: %w", err)
		}
		w = aw
	}
	enc, err := audio.NewPCMEncoderWriter(codec.PayloadType, w)
	if err != nil {
		return nil, fmt.Errorf("encoder for %s: %w", codec.Name, err)
	}
	e.enc = enc

	// Reader: prefer the DTMF-aware reader so keypad events are reported while audio is read.
	var r io.Reader
	if dr, err := med.AudioReaderDTMF(); err == nil {
		dr.OnDTMF(func(d rune) error {
			e.digitsMu.Lock()
			e.digits = append(e.digits, d)
			e.digitsMu.Unlock()
			return nil
		})
		r = dr
	} else {
		ar, err := med.AudioReader()
		if err != nil {
			return nil, fmt.Errorf("audio reader: %w", err)
		}
		r = ar
	}
	dec, err := audio.NewPCMDecoderReader(codec.PayloadType, r)
	if err != nil {
		return nil, fmt.Errorf("decoder for %s: %w", codec.Name, err)
	}
	go e.read(dec)
	go e.pace()
	return e, nil
}

func (e *Engine) pace() {
	frame := make([]int16, tone.FrameSamples)
	b := make([]byte, tone.FrameSamples*2)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-ticker.C:
		}
		if time.Now().UnixNano() < e.echoUntil.Load() {
			continue // the read loop writes echoed frames
		}
		e.toneMu.Lock()
		g := e.toneGen
		if g != nil && time.Now().After(e.toneEnd) {
			g, e.toneGen = nil, nil
		}
		if g != nil {
			g.Frame(frame)
		} else {
			clear(frame)
		}
		e.toneMu.Unlock()
		for i, v := range frame {
			binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
		}
		e.writeMu.Lock()
		_, err := e.enc.Write(b)
		e.writeMu.Unlock()
		if err != nil {
			e.writeErr.Store(&err)
			return
		}
	}
}

func (e *Engine) read(dec io.Reader) {
	defer close(e.done)
	buf := make([]byte, media.RTPBufSize*4)
	samples := make([]int16, 0, len(buf)/2)
	for {
		n, err := dec.Read(buf)
		if n > 0 {
			samples = samples[:0]
			for i := 0; i+1 < n; i += 2 {
				samples = append(samples, int16(binary.LittleEndian.Uint16(buf[i:i+2])))
			}
			e.detector.Feed(samples)
			e.frames.Add(1)
			if time.Now().UnixNano() < e.echoUntil.Load() {
				e.writeMu.Lock()
				_, _ = e.enc.Write(buf[:n])
				e.writeMu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// Codec is the negotiated audio codec's name, e.g. "PCMU".
func (e *Engine) Codec() string { return codecName(e.codec) }

// Heard returns the frequencies present for at least minFrames 20 ms frames.
func (e *Engine) Heard(minFrames int) []float64 { return e.detector.Heard(minFrames) }

// Digits returns the keypad tones received so far, in order.
func (e *Engine) Digits() string {
	e.digitsMu.Lock()
	defer e.digitsMu.Unlock()
	return string(e.digits)
}

// FramesReceived is the number of audio reads with data; zero means no media arrived.
func (e *Engine) FramesReceived() int64 { return e.frames.Load() }

// Done is closed when the media stream ends.
func (e *Engine) Done() <-chan struct{} { return e.done }

// PlayTone plays hz for d, paced in real time. It returns early if ctx ends.
func (e *Engine) PlayTone(ctx context.Context, hz float64, d time.Duration) error {
	if err := e.writeErr.Load(); err != nil {
		return fmt.Errorf("writing tone: %w", *err)
	}
	e.toneMu.Lock()
	e.toneGen = tone.NewGenerator(hz, ToneAmplitude)
	e.toneEnd = time.Now().Add(d)
	e.toneMu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	e.toneMu.Lock()
	e.toneGen = nil
	e.toneMu.Unlock()
	if err := e.writeErr.Load(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("writing tone: %w", *err)
	}
	return nil
}

// SendDigits sends keypad tones as RFC 2833 events.
func (e *Engine) SendDigits(ctx context.Context, digits string, gap time.Duration) error {
	if e.dtmf == nil {
		return fmt.Errorf("telephone-event was not negotiated on this call")
	}
	if gap <= 0 {
		gap = 100 * time.Millisecond
	}
	for _, d := range digits {
		if err := e.dtmf.WriteDTMF(d); err != nil {
			return fmt.Errorf("sending digit %q: %w", d, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(gap):
		}
	}
	return nil
}

// Echo plays back received audio for d.
func (e *Engine) Echo(ctx context.Context, d time.Duration) {
	e.echoUntil.Store(time.Now().Add(d).UnixNano())
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	e.echoUntil.Store(0)
}

// RunMedia runs one media step. Signaling steps return an error; the caller handles those itself.
// hangup is called for a hangup step.
func (e *Engine) RunMedia(ctx context.Context, s Step, hangup func() error) error {
	switch {
	case s.Tone != nil:
		return e.PlayTone(ctx, s.Tone.Hz, ms(s.Tone.Ms))
	case s.Digits != nil:
		return e.SendDigits(ctx, s.Digits.Digits, ms(s.Digits.GapMs))
	case s.Wait != nil:
		select {
		case <-ctx.Done():
		case <-time.After(ms(s.Wait.Ms)):
		}
		return nil
	case s.Echo != nil:
		e.Echo(ctx, ms(s.Echo.Ms))
		return nil
	case s.Hangup != nil:
		return hangup()
	}
	return fmt.Errorf("%s is not a media step", s.Kind())
}

func codecName(c media.Codec) string {
	switch c.PayloadType {
	case 0:
		return "PCMU"
	case 8:
		return "PCMA"
	case 9:
		return "G722"
	}
	return c.Name
}
