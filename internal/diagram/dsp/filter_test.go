package dsp

import (
	"math"
	"testing"
)

const rate = 5000.0

// tone measures the amplitude of one frequency in a signal by correlating with a
// sine and cosine over whole periods, so a leftover of the other tone does not count.
func tone(x []float64, f float64) float64 {
	var c, s float64
	for i, v := range x {
		w := 2 * math.Pi * f * float64(i) / rate
		c += v * math.Cos(w)
		s += v * math.Sin(w)
	}
	return 2 * math.Hypot(c, s) / float64(len(x))
}

func db(r float64) float64 { return 20 * math.Log10(r) }

// both returns the 50 Hz plus 500 Hz signal, 1 second long at 5 kHz, and the tail
// after the filter has settled (whole periods of both tones fit in the last 1000).
func both() []float64 {
	x := make([]float64, int(rate))
	for i := range x {
		x[i] = math.Sin(2*math.Pi*50*float64(i)/rate) + math.Sin(2*math.Pi*500*float64(i)/rate)
	}
	return x
}

func lastN(x []float64, n int) []float64 { return x[len(x)-n:] }

// A2 acceptance: 50 Hz plus 500 Hz through a 4th order lowpass at 100 Hz loses at
// least 40 dB of the 500 Hz tone and the 50 Hz tone stays within 0.5 dB.
//
// The closed form for a bilinear Butterworth lowpass is
// |H|^2 = 1/(1+(tan(pi f/fs)/tan(pi fc/fs))^(2n)), the analog 1/(1+(f/fc)^(2n)) on
// the warped frequency axis. At 500 Hz, n=4, fc=100, fs=5000 it is -57.04 dB (the
// unwarped analog figure is -55.92 dB). At 50 Hz it is -0.017 dB.
func TestLowpassRemovesTheHighTone(t *testing.T) {
	secs, err := Design(Lowpass, 4, rate, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	y, err := Filter(both(), secs)
	if err != nil {
		t.Fatal(err)
	}
	tail := lastN(y, 2000)
	hi := db(tone(tail, 500))
	lo := db(tone(tail, 50))
	t.Logf("500 Hz: %.2f dB, 50 Hz: %.3f dB", hi, lo)
	if hi > -40 {
		t.Errorf("500 Hz only fell to %.2f dB, want -40 or lower", hi)
	}
	if math.Abs(lo) > 0.5 {
		t.Errorf("50 Hz moved by %.3f dB, want within 0.5", lo)
	}
	want := -10 * math.Log10(1+math.Pow(math.Tan(math.Pi*500/rate)/math.Tan(math.Pi*100/rate), 8))
	near(t, "500 Hz against the closed form", hi, want, 0.01)
	// The response from the library agrees with the closed form at 100 Hz: -3.01 dB.
	r, err := Response(secs, rate, []float64{100, 500})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "the cutoff is -3 dB", r[0], -10*math.Log10(2), 1e-9)
	near(t, "response at 500 Hz", r[1], want, 1e-9)
}

// Every Butterworth is -3.0103 dB at its cutoff, whatever the order, because the
// edge is prewarped. The closed form is 10*log10(1/2).
func TestCutoffIsThreeDBAtEveryOrder(t *testing.T) {
	for n := 1; n <= MaxOrder; n++ {
		for _, fc := range []float64{50, 400, 1200} {
			lp, err := Design(Lowpass, n, rate, fc, 0)
			if err != nil {
				t.Fatal(err)
			}
			hp, _ := Design(Highpass, n, rate, fc, 0)
			rl, _ := Response(lp, rate, []float64{fc})
			rh, _ := Response(hp, rate, []float64{fc})
			near(t, "lowpass edge", rl[0], -3.0103, 0.001)
			near(t, "highpass edge", rh[0], -3.0103, 0.001)
		}
	}
}

// The mirror: a 4th order highpass at 100 Hz removes 50 Hz (closed form
// 1/(1+(tan(pi 100/fs)/tan(pi 50/fs))^8), about -24 dB) and keeps 500 Hz within 0.5 dB.
func TestHighpassIsTheMirror(t *testing.T) {
	secs, err := Design(Highpass, 4, rate, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := Filter(both(), secs)
	tail := lastN(y, 2000)
	lo, hi := db(tone(tail, 50)), db(tone(tail, 500))
	want := -10 * math.Log10(1+math.Pow(math.Tan(math.Pi*100/rate)/math.Tan(math.Pi*50/rate), 8))
	t.Logf("50 Hz: %.2f dB (closed form %.2f), 500 Hz: %.3f dB", lo, want, hi)
	near(t, "50 Hz against the closed form", lo, want, 0.01)
	if lo > -20 {
		t.Errorf("50 Hz only fell to %.2f dB", lo)
	}
	if math.Abs(hi) > 0.5 {
		t.Errorf("500 Hz moved by %.3f dB", hi)
	}
}

// A bandpass from 400 to 600 Hz of order 3 passes 500 Hz, and rejects both sides.
func TestBandpassPassesItsBand(t *testing.T) {
	secs, err := Design(Bandpass, 3, rate, 400, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 3 {
		t.Errorf("order 3 bandpass has %d sections, want 3", len(secs))
	}
	r, _ := Response(secs, rate, []float64{50, 400, 490, 600, 1500})
	t.Logf("50 Hz %.1f, 400 Hz %.2f, 490 Hz %.3f, 600 Hz %.2f, 1500 Hz %.1f dB", r[0], r[1], r[2], r[3], r[4])
	near(t, "lower edge", r[1], -3.0103, 0.01)
	near(t, "upper edge", r[3], -3.0103, 0.01)
	if r[2] < -0.5 {
		t.Errorf("inside the band the gain is %.3f dB", r[2])
	}
	if r[0] > -40 || r[4] > -40 {
		t.Errorf("rejection below %.1f dB and above %.1f dB, want -40 or lower", r[0], r[4])
	}
	// And in a time signal: 50 and 500 Hz in, only 500 Hz out.
	y, _ := Filter(both(), secs)
	tail := lastN(y, 2000)
	if db(tone(tail, 50)) > -40 {
		t.Errorf("50 Hz leaked at %.1f dB", db(tone(tail, 50)))
	}
	if math.Abs(db(tone(tail, 500))) > 0.5 {
		t.Errorf("500 Hz moved by %.2f dB", db(tone(tail, 500)))
	}
}

// A bandstop around 50 Hz notches it out of 50 plus 500, and 500 survives.
func TestBandstopNotchesATone(t *testing.T) {
	secs, err := Design(Bandstop, 3, rate, 45, 55)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := Response(secs, rate, []float64{0, 45, 50, 55, 500, 2500})
	t.Logf("0 Hz %.3f, 45 Hz %.2f, 50 Hz %.1f, 55 Hz %.2f, 500 Hz %.3f, 2500 Hz %.3f dB", r[0], r[1], r[2], r[3], r[4], r[5])
	near(t, "gain at DC", r[0], 0, 0.01)
	near(t, "lower edge", r[1], -3.0103, 0.01)
	near(t, "upper edge", r[3], -3.0103, 0.01)
	near(t, "gain at the highest frequency", r[5], 0, 0.01)
	y, _ := Filter(both(), secs)
	tail := lastN(y, 2000)
	if got := db(tone(tail, 50)); got > -40 {
		t.Errorf("50 Hz only fell to %.1f dB", got)
	}
	if got := db(tone(tail, 500)); math.Abs(got) > 0.1 {
		t.Errorf("500 Hz moved by %.3f dB", got)
	}
}

// A step in settles to the step's height: the DC gain of a lowpass is 1, and the
// settled output of a highpass is 0.
func TestStepResponseSettles(t *testing.T) {
	step := make([]float64, 5000)
	for i := range step {
		step[i] = 1
	}
	lp, _ := Design(Lowpass, 6, rate, 100, 0)
	y, _ := Filter(step, lp)
	near(t, "lowpass settles to 1", y[len(y)-1], 1, 1e-6)
	hp, _ := Design(Highpass, 6, rate, 100, 0)
	y, _ = Filter(step, hp)
	near(t, "highpass settles to 0", y[len(y)-1], 0, 1e-6)
	// A Butterworth lowpass does not ring much: second order overshoot is 4.3 percent.
	lp2, _ := Design(Lowpass, 2, rate, 100, 0)
	y, _ = Filter(step, lp2)
	peak, _ := Max(y)
	near(t, "second order overshoot", peak, 1.0432, 0.002)
}

// Cutting a signal into chunks gives the same samples as filtering it whole.
func TestStreamingMatchesWhole(t *testing.T) {
	x := both()[:1000]
	secs, _ := Design(Bandpass, 2, rate, 300, 700)
	whole, _ := Filter(x, secs)
	st := NewStream(secs)
	var got []float64
	for i := 0; i < len(x); i += 77 {
		j := i + 77
		if j > len(x) {
			j = len(x)
		}
		got = append(got, st.Process(x[i:j])...)
	}
	for i := range whole {
		if got[i] != whole[i] {
			t.Fatalf("sample %d: chunked %v, whole %v", i, got[i], whole[i])
		}
	}
}

func TestEveryDesignIsStable(t *testing.T) {
	kinds := []Kind{Lowpass, Highpass, Bandpass, Bandstop}
	for _, k := range kinds {
		for n := 1; n <= MaxOrder; n++ {
			for _, f := range []float64{1, 10, 100, 1000, 2400} {
				f2 := 0.0
				if k == Bandpass || k == Bandstop {
					f2 = f * 1.2
					if f2 >= rate/2 {
						continue
					}
				}
				secs, err := Design(k, n, rate, f, f2)
				if err != nil {
					t.Errorf("%s order %d at %g: %v", k, n, f, err)
					continue
				}
				if !Stable(secs) {
					t.Errorf("%s order %d at %g is not stable", k, n, f)
				}
				var want int
				if k == Bandpass || k == Bandstop {
					want = n
				} else {
					want = (n + 1) / 2
				}
				if len(secs) != want {
					t.Errorf("%s order %d has %d sections, want %d", k, n, len(secs), want)
				}
			}
		}
	}
}

func TestDesignRefusesBadValues(t *testing.T) {
	cases := map[string]struct {
		k      Kind
		n      int
		r      float64
		f1, f2 float64
	}{
		"order 0":            {Lowpass, 0, 1000, 100, 0},
		"order 11":           {Lowpass, 11, 1000, 100, 0},
		"cutoff zero":        {Lowpass, 2, 1000, 0, 0},
		"cutoff negative":    {Highpass, 2, 1000, -5, 0},
		"cutoff at nyquist":  {Lowpass, 2, 1000, 500, 0},
		"cutoff above":       {Lowpass, 2, 1000, 900, 0},
		"rate zero":          {Lowpass, 2, 0, 10, 0},
		"rate nan":           {Lowpass, 2, math.NaN(), 10, 0},
		"cutoff nan":         {Lowpass, 2, 1000, math.NaN(), 0},
		"band reversed":      {Bandpass, 2, 1000, 300, 200},
		"band equal":         {Bandstop, 2, 1000, 200, 200},
		"band upper nyquist": {Bandpass, 2, 1000, 100, 500},
		"band lower zero":    {Bandpass, 2, 1000, 0, 100},
		"unknown kind":       {"notch", 2, 1000, 100, 0},
	}
	for name, c := range cases {
		if _, err := Design(c.k, c.n, c.r, c.f1, c.f2); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	secs, _ := Design(Lowpass, 2, 1000, 100, 0)
	if _, err := Filter(nil, secs); err == nil {
		t.Error("an empty array filtered without an error")
	}
	if _, err := Filter([]float64{1, math.NaN()}, secs); err == nil {
		t.Error("NaN filtered without an error")
	}
	if _, err := Filter([]float64{1}, nil); err == nil {
		t.Error("no sections filtered without an error")
	}
	if _, err := Response(secs, 1000, []float64{600}); err == nil {
		t.Error("a frequency above Nyquist was accepted")
	}
}

// Without prewarping the -3 dB point lands in the wrong place and the 40 dB check
// (written for a cutoff of 100 Hz at 5 kHz) is the one that shows it at high edges.
func TestPrewarpingMovesTheEdgeToWhereItWasAsked(t *testing.T) {
	secs, _ := Design(Lowpass, 4, 1000, 400, 0)
	r, _ := Response(secs, 1000, []float64{400})
	near(t, "edge near Nyquist", r[0], -3.0103, 0.001)
}

// The one the acceptance table asks for, at a rate where the transform's warping is large:
// 50 Hz plus 500 Hz sampled at 1.2 kHz, lowpass at 100 Hz. Closed form again.
func TestLowpassAtALowRate(t *testing.T) {
	const r = 1200.0
	x := make([]float64, 2400)
	for i := range x {
		x[i] = math.Sin(2*math.Pi*50*float64(i)/r) + math.Sin(2*math.Pi*500*float64(i)/r)
	}
	secs, err := Design(Lowpass, 4, r, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := Filter(x, secs)
	tail := y[len(y)-1200:]
	var c, s float64
	for i, v := range tail {
		w := 2 * math.Pi * 500 * float64(i) / r
		c += v * math.Cos(w)
		s += v * math.Sin(w)
	}
	got := db(2 * math.Hypot(c, s) / float64(len(tail)))
	t.Logf("500 Hz at 1.2 kHz: %.1f dB", got)
	if got > -40 {
		t.Errorf("500 Hz only fell to %.1f dB", got)
	}
}

func FuzzFilter(f *testing.F) {
	f.Add(uint8(0), uint8(4), 5000.0, 100.0, 200.0, int64(1), 64)
	f.Add(uint8(2), uint8(10), 1000.0, 10.0, 20.0, int64(7), 300)
	f.Add(uint8(3), uint8(1), 44100.0, 50.0, 60.0, int64(3), 1)
	f.Fuzz(func(t *testing.T, kind, order uint8, rate, f1, f2 float64, seed int64, n int) {
		kinds := []Kind{Lowpass, Highpass, Bandpass, Bandstop}
		secs, err := Design(kinds[int(kind)%4], int(order), rate, f1, f2)
		if err != nil {
			return // refused inputs are fine; only a returned design must be good
		}
		if !Stable(secs) {
			t.Fatalf("an unstable design came back: %+v", secs)
		}
		for _, s := range secs {
			for _, v := range []float64{s.B0, s.B1, s.B2, s.A1, s.A2} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("a coefficient is %v in %+v", v, secs)
				}
			}
		}
		if n < 1 {
			n = 1
		}
		if n > 4096 {
			n = 4096
		}
		x := make([]float64, n)
		st := uint64(seed)
		for i := range x {
			st = st*6364136223846793005 + 1442695040888963407
			x[i] = float64(int64(st>>11)%2001-1000) / 10
		}
		y, err := Filter(x, secs)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range y {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("output %d is %v", i, v)
			}
		}
		fr := []float64{0, rate / 4, rate / 2}
		resp, err := Response(secs, rate, fr)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range resp {
			if math.IsNaN(v) {
				t.Fatalf("a response is NaN for %+v", secs)
			}
		}
	})
}

// A cutoff that is a large share of the sample rate is where an unwarped design
// goes wrong: lowpass at 400 Hz at 1 kHz must pass a 300 Hz tone within 0.5 dB
// (closed form with the warped axis: -0.007 dB) and must be 3 dB down at 400 Hz.
func TestPassbandHoldsWhenTheCutoffIsNearNyquist(t *testing.T) {
	const r = 1000.0
	x := make([]float64, 4000)
	for i := range x {
		x[i] = math.Sin(2 * math.Pi * 300 * float64(i) / r)
	}
	secs, err := Design(Lowpass, 4, r, 400, 0)
	if err != nil {
		t.Fatal(err)
	}
	y, _ := Filter(x, secs)
	tail := y[len(y)-2000:]
	var c, s float64
	for i, v := range tail {
		w := 2 * math.Pi * 300 * float64(i) / r
		c += v * math.Cos(w)
		s += v * math.Sin(w)
	}
	got := db(2 * math.Hypot(c, s) / float64(len(tail)))
	want := -10 * math.Log10(1+math.Pow(math.Tan(math.Pi*300/r)/math.Tan(math.Pi*400/r), 8))
	t.Logf("300 Hz through a 400 Hz lowpass at 1 kHz: %.4f dB (closed form %.4f)", got, want)
	near(t, "300 Hz against the closed form", got, want, 0.01)
	if math.Abs(got) > 0.5 {
		t.Errorf("300 Hz moved by %.3f dB", got)
	}
	e, _ := Response(secs, r, []float64{400})
	near(t, "edge", e[0], -3.0103, 0.001)
}
