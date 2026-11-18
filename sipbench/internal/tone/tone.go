// Package tone generates test tones and detects them in received audio.
//
// The bench proves two-way audio by having each end play a tone at a frequency only it uses, and listening for
// the other end's tone. Detection uses the Goertzel algorithm on 20 ms frames of 8 kHz linear PCM.
package tone

import (
	"maps"
	"math"
	"slices"
	"sync"
	"time"
)

// SampleRate is the rate every bench codec decodes to (G.711).
const SampleRate = 8000

// FrameSamples is one 20 ms frame at SampleRate.
const FrameSamples = SampleRate / 50

// Generator produces a continuous sine wave in 20 ms frames, keeping phase between frames.
type Generator struct {
	hz        float64
	amplitude float64
	phase     float64
}

// NewGenerator returns a generator for hz at the given peak amplitude (0..32767).
func NewGenerator(hz float64, amplitude float64) *Generator {
	return &Generator{hz: hz, amplitude: amplitude}
}

// Frame fills out with the next len(out) samples.
func (g *Generator) Frame(out []int16) {
	step := 2 * math.Pi * g.hz / SampleRate
	for i := range out {
		out[i] = int16(g.amplitude * math.Sin(g.phase))
		g.phase += step
		if g.phase > 2*math.Pi {
			g.phase -= 2 * math.Pi
		}
	}
}

// Ratio returns how much of the frame's energy sits at hz, between 0 and 1. A clean sine at hz gives about 1.
func Ratio(samples []int16, hz float64) float64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	coeff := 2 * math.Cos(2*math.Pi*hz/SampleRate)
	var s1, s2, energy float64
	for _, v := range samples {
		x := float64(v)
		energy += x * x
		s0 := x + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	if energy == 0 {
		return 0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	return 2 * power / (float64(n) * energy)
}

// RMS returns the root mean square level of the frame.
func RMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, v := range samples {
		x := float64(v)
		sum += x * x
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// Detector listens for a set of frequencies across a call and counts the frames in which each was present.
// It is safe for concurrent use.
type Detector struct {
	freqs     []float64
	threshold float64
	minRMS    float64

	mu     sync.Mutex
	frames map[float64]int
	buf    []int16
	// For each frequency: when a frame last held it, and the longest wait between two such frames. Timed by
	// arrival, so a stretch in which no packets came at all counts as a gap too.
	last   map[float64]time.Time
	first  map[float64]time.Time
	maxGap map[float64]time.Duration
	now    func() time.Time
}

// NewDetector listens for freqs. A frame counts for a frequency when at least threshold of its energy sits
// there and its level is above minRMS (so silence and comfort noise never count).
func NewDetector(freqs []float64) *Detector {
	return &Detector{
		freqs:     append([]float64(nil), freqs...),
		threshold: 0.6,
		minRMS:    200,
		frames:    map[float64]int{},
		last:      map[float64]time.Time{},
		first:     map[float64]time.Time{},
		maxGap:    map[float64]time.Duration{},
		now:       time.Now,
	}
}

// Feed adds received samples. They are cut into 20 ms frames internally.
func (d *Detector) Feed(samples []int16) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, samples...)
	now := d.now()
	for len(d.buf) >= FrameSamples {
		frame := d.buf[:FrameSamples]
		if RMS(frame) >= d.minRMS {
			for _, f := range d.freqs {
				if Ratio(frame, f) >= d.threshold {
					d.frames[f]++
					if last, ok := d.last[f]; ok && now.Sub(last) > d.maxGap[f] {
						d.maxGap[f] = now.Sub(last)
					}
					d.last[f] = now
					if _, ok := d.first[f]; !ok {
						d.first[f] = now
					}
				}
			}
		}
		d.buf = d.buf[FrameSamples:]
	}
}

// Heard returns the frequencies present for at least minFrames frames (each frame is 20 ms), in ascending order.
func (d *Detector) Heard(minFrames int) []float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []float64
	for f, n := range d.frames {
		if n >= minFrames {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// MaxGaps returns, for each frequency heard, the longest time between two frames that held it: how long the tone
// was lost for at worst, once it had started. A far end that plays a tone throughout should show only jitter here.
func (d *Detector) MaxGaps() map[float64]time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[float64]time.Duration, len(d.last))
	for f := range d.last {
		out[f] = d.maxGap[f]
	}
	return out
}

// Frames returns how many frames each frequency was present for.
func (d *Detector) Frames() map[float64]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[float64]int, len(d.frames))
	maps.Copy(out, d.frames)
	return out
}

// First returns when each frequency heard was first detected.
func (d *Detector) First() map[float64]time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.first)
}

// Coverage returns, for each frequency heard, the share of 20 ms frames between its first and last detection (by
// arrival) in which it was present: 1 for a tone that arrived whole, less for one with frames lost on the way.
// Duplicated packets can push it past 1.
func (d *Detector) Coverage() map[float64]float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	frame := time.Second / 50
	out := make(map[float64]float64, len(d.first))
	for f, first := range d.first {
		span := d.last[f].Sub(first) + frame
		out[f] = float64(d.frames[f]) * float64(frame) / float64(span)
	}
	return out
}
