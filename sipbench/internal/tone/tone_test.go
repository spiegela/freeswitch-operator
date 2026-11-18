package tone

import (
	"math/rand"
	"testing"
	"time"
)

func frames(hz float64, amplitude float64, n int) []int16 {
	g := NewGenerator(hz, amplitude)
	out := make([]int16, FrameSamples*n)
	g.Frame(out)
	return out
}

func TestRatioPureTone(t *testing.T) {
	for _, hz := range []float64{600, 1000, 1400, 1800} {
		s := frames(hz, 8000, 1)
		if r := Ratio(s, hz); r < 0.9 {
			t.Errorf("%v Hz: ratio at own frequency = %.2f, want >= 0.9", hz, r)
		}
		for _, other := range []float64{600, 1000, 1400, 1800} {
			if other == hz {
				continue
			}
			if r := Ratio(s, other); r > 0.2 {
				t.Errorf("%v Hz tone: ratio at %v Hz = %.2f, want <= 0.2", hz, other, r)
			}
		}
	}
}

func TestDetectorHeardAndSilence(t *testing.T) {
	d := NewDetector([]float64{600, 1000, 1400})
	d.Feed(frames(1000, 6000, 25)) // 500 ms of 1000 Hz
	d.Feed(make([]int16, FrameSamples*25))
	got := d.Heard(10)
	if len(got) != 1 || got[0] != 1000 {
		t.Fatalf("Heard = %v, want [1000]", got)
	}
}

func TestDetectorIgnoresNoise(t *testing.T) {
	d := NewDetector([]float64{600, 1000, 1400})
	r := rand.New(rand.NewSource(1))
	noise := make([]int16, FrameSamples*50)
	for i := range noise {
		noise[i] = int16(r.Intn(8000) - 4000)
	}
	d.Feed(noise)
	if got := d.Heard(3); len(got) != 0 {
		t.Fatalf("Heard in noise = %v, want none", got)
	}
}

func TestDetectorToneWithNoise(t *testing.T) {
	d := NewDetector([]float64{600})
	r := rand.New(rand.NewSource(2))
	s := frames(600, 8000, 30)
	for i := range s {
		s[i] += int16(r.Intn(1000) - 500)
	}
	// Feed in odd-sized chunks to exercise framing.
	for len(s) > 0 {
		n := min(97, len(s))
		d.Feed(s[:n])
		s = s[n:]
	}
	if got := d.Heard(20); len(got) != 1 {
		t.Fatalf("Heard = %v (frames %v), want [600]", got, d.Frames())
	}
}

func TestDetectorMaxGaps(t *testing.T) {
	d := NewDetector([]float64{1000})
	clock := time.Unix(0, 0)
	d.now = func() time.Time { return clock }
	g := NewGenerator(1000, 8000)
	frame := make([]int16, FrameSamples)
	feed := func(n int, step time.Duration) {
		for range n {
			g.Frame(frame)
			d.Feed(frame)
			clock = clock.Add(step)
		}
	}
	feed(50, 20*time.Millisecond)
	clock = clock.Add(1500 * time.Millisecond) // packets stop for 1.5 s
	feed(50, 20*time.Millisecond)
	gap := d.MaxGaps()[1000]
	if gap < 1500*time.Millisecond || gap > 1600*time.Millisecond {
		t.Fatalf("max gap %v, want about 1.52s", gap)
	}
	if _, ok := d.MaxGaps()[600]; ok {
		t.Fatal("a tone never heard has no gap")
	}
}

func TestDetectorCoverage(t *testing.T) {
	d := NewDetector([]float64{1000})
	start := time.Unix(0, 0)
	clock := start
	d.now = func() time.Time { return clock }
	g := NewGenerator(1000, 8000)
	frame := make([]int16, FrameSamples)
	// 100 frames' time, one in ten lost on the way: 90 arrive.
	for i := range 100 {
		g.Frame(frame)
		if i%10 != 5 {
			d.Feed(frame)
		}
		clock = clock.Add(20 * time.Millisecond)
	}
	if c := d.Coverage()[1000]; c < 0.89 || c > 0.91 {
		t.Fatalf("coverage %.3f, want 0.9", c)
	}
	if f := d.First()[1000]; !f.Equal(start) {
		t.Fatalf("first heard at %v, want %v", f, start)
	}
}
