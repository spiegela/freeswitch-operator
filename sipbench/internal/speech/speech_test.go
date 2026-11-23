package speech

import (
	"math"
	"slices"
	"testing"

	"github.com/spiegela/freeswitch-operator/sipbench/internal/tone"
)

func TestSpeechLevel(t *testing.T) {
	g := NewGenerator(1)
	frame := make([]int16, tone.FrameSamples)
	peak, sum, n := 0.0, 0.0, 0
	for range 50 * 20 { // 20 s
		g.Frame(frame)
		for _, s := range frame {
			v := math.Abs(float64(s))
			peak = math.Max(peak, v)
			sum += v * v
			n++
		}
	}
	rms := math.Sqrt(sum / float64(n))
	t.Logf("peak %.0f, rms %.0f", peak, rms)
	if peak < Peak*0.6 || peak >= 32767 {
		t.Errorf("peak %.0f, want near %d and unclipped", peak, Peak)
	}
	if rms < 1000 {
		t.Errorf("rms %.0f: too quiet to be heard as talking", rms)
	}
}

func TestSpeechIsRepeatable(t *testing.T) {
	a, b, c := NewGenerator(7), NewGenerator(7), NewGenerator(8)
	fa, fb, fc := make([]int16, 4000), make([]int16, 4000), make([]int16, 4000)
	a.Frame(fa)
	b.Frame(fb)
	c.Frame(fc)
	if !slices.Equal(fa, fb) {
		t.Error("the same seed gave different speech")
	}
	if slices.Equal(fa, fc) {
		t.Error("different seeds gave the same speech")
	}
}

// Speech must not pass for any of the bench's tones, or an end that hears it would report a tone it never got.
func TestSpeechIsNoTone(t *testing.T) {
	freqs := []float64{350, 440, 600, 700, 800, 1000, 1200, 1400, 1600, 2000}
	d := tone.NewDetector(freqs)
	g := NewGenerator(3)
	frame := make([]int16, tone.FrameSamples)
	for range 50 * 30 {
		g.Frame(frame)
		d.Feed(frame)
	}
	if heard := d.Heard(10); len(heard) > 0 {
		t.Errorf("30 s of speech passed for tones %v", heard)
	}
}
