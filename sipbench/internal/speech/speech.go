// Package speech synthesises audio that voice activity detectors take for someone talking.
//
// The bench's tones prove two-way audio but are not speech: a voice detector (Silero, WebRTC's) ignores them. To
// test barge-in detection the bench needs a caller, or an agent, that talks. Rather than ship a recording, this
// is a small formant synthesiser: a glottal pulse train at a speaking pitch, shaped by three vocal-tract
// resonances that glide from vowel to vowel, in syllables of 160-280 ms with a breathy onset now and then. Silero
// scores it 0.97-0.99, as it does recorded speech. It is deterministic for a seed, so runs are repeatable.
package speech

import (
	"math"
	"math/rand/v2"
)

// SampleRate is the rate generated audio is at (G.711's, as every bench codec decodes to).
const SampleRate = 8000

// Peak is about the loudest sample generated, near -9 dBFS: talking level, a little louder than the bench's tones.
const Peak = 12000

// gain scales the synthesiser's raw output to Peak: its raw peak sits near 10 for every seed tried.
const gain = Peak / 10.0

// vowels are the first three formants (Hz) of a handful of English vowels: /a/ /i/ /u/ /e/ /o/ /ae/ /schwa/.
var vowels = [][3]float64{
	{730, 1090, 2440}, {270, 2290, 3010}, {300, 870, 2240}, {530, 1840, 2480},
	{570, 840, 2410}, {660, 1720, 2410}, {440, 1020, 2240},
}

// resonator is a two-pole band-pass filter: one formant.
type resonator struct{ a, b, c, y1, y2 float64 }

func (r *resonator) tune(hz, bandwidth float64) {
	t := 1.0 / SampleRate
	r.c = -math.Exp(-2 * math.Pi * bandwidth * t)
	r.b = 2 * math.Exp(-math.Pi*bandwidth*t) * math.Cos(2*math.Pi*hz*t)
	r.a = 1 - r.b - r.c
}

func (r *resonator) step(x float64) float64 {
	y := r.a*x + r.b*r.y1 + r.c*r.y2
	r.y2, r.y1 = r.y1, y
	return y
}

// Generator produces continuous speech, frame by frame. Pauses are the caller's business: stop asking for frames.
type Generator struct {
	rng      *rand.Rand
	formants [3]resonator
	// tilt is the glottal source's spectral roll-off: two one-pole low-passes.
	tilt1, tilt2 float64
	phase        float64

	// The syllable being spoken.
	from, to [3]float64
	pitch    float64
	breathy  bool
	n, i     int // its length and position, in samples
}

// NewGenerator returns a generator; the same seed gives the same speech.
func NewGenerator(seed uint64) *Generator {
	g := &Generator{rng: rand.New(rand.NewPCG(seed, 0x5eed))}
	g.to = vowels[0]
	g.next()
	return g
}

// next starts a syllable: a new vowel, length, pitch and whether it opens with breath.
func (g *Generator) next() {
	g.from = g.to
	g.to = vowels[g.rng.IntN(len(vowels))]
	g.n = int((0.16 + 0.12*g.rng.Float64()) * SampleRate)
	g.i = 0
	g.pitch = 105 + 30*g.rng.Float64()
	g.breathy = g.rng.IntN(3) == 0
}

// Frame fills out with the next len(out) samples.
func (g *Generator) Frame(out []int16) {
	for k := range out {
		if g.i >= g.n {
			g.next()
		}
		t := float64(g.i) / float64(g.n)
		g.i++

		// The formants glide to the new vowel over the first 30% of the syllable.
		glide := math.Min(1, t/0.3)
		for f := range g.formants {
			g.formants[f].tune(g.from[f]+(g.to[f]-g.from[f])*glide, 60+40*float64(f))
		}
		// Pitch rises and falls over the syllable, with a little jitter.
		f0 := g.pitch * (1 + 0.15*math.Sin(math.Pi*t)) * (1 + 0.01*g.rng.NormFloat64())
		g.phase += f0 / SampleRate
		pulse := 0.0
		if g.phase >= 1 {
			g.phase--
			pulse = 1
		}
		g.tilt1 = 0.9*g.tilt1 + pulse
		g.tilt2 = 0.9*g.tilt2 + g.tilt1
		v := g.formants[2].step(g.formants[1].step(g.formants[0].step(g.tilt2)))

		// Each syllable swells and fades.
		env := math.Pow(math.Max(math.Sin(math.Pi*math.Min(1, t*1.2)), 0), 0.6)
		v *= env
		if g.breathy && t < 0.2 {
			v = v*t/0.2 + 0.3*g.rng.NormFloat64()*(1-t/0.2)
		}
		out[k] = int16(math.Max(-32767, math.Min(32767, v*gain)))
	}
}
