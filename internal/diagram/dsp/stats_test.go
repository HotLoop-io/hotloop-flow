package dsp

import (
	"math"
	"strings"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.12g, want %.12g (tolerance %g)", name, got, want, tol)
	}
}

func sine(amp, freq, rate float64, n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = amp * math.Sin(2*math.Pi*freq*float64(i)/rate)
	}
	return x
}

// Golden vector, computed by hand: 2, 4, 4, 4, 5, 5, 7, 9 has sum 40 so the mean
// is 5. The squared deviations are 9, 1, 1, 1, 0, 0, 4, 16, which sum to 32, so the
// sample variance is 32/7 and the standard deviation sqrt(32/7) = 2.13808993529939.
func TestGoldenStatistics(t *testing.T) {
	x := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	m, _ := Mean(x)
	near(t, "mean", m, 5, 0)
	sd, _ := StdDev(x)
	near(t, "stddev", sd, 2.13808993529939, 1e-13)
	lo, _ := Min(x)
	hi, _ := Max(x)
	near(t, "min", lo, 2, 0)
	near(t, "max", hi, 9, 0)
	// sum of squares = 4+16+16+16+25+25+49+81 = 232, so RMS = sqrt(232/8) = sqrt(29).
	r, _ := RMS(x)
	near(t, "rms", r, math.Sqrt(29), 1e-14)
}

// A1 acceptance: a known sine, RMS equals the amplitude over root 2 to 6 digits.
func TestRMSOfASineIsAmplitudeOverRootTwo(t *testing.T) {
	for _, amp := range []float64{1, 2.5, 0.001, 1000} {
		x := sine(amp, 50, 10000, 2000) // 10 whole periods
		r, err := RMS(x)
		if err != nil {
			t.Fatal(err)
		}
		want := amp / math.Sqrt2
		if math.Abs(r-want)/want > 1e-6 {
			t.Errorf("amplitude %g: rms %.9g, want %.9g", amp, r, want)
		}
		m, _ := Mean(x)
		near(t, "mean of a sine", m, 0, 1e-12*amp)
		// The sample std dev of a whole-period sine is rms * sqrt(n/(n-1)).
		sd, _ := StdDev(x)
		near(t, "stddev of a sine", sd, want*math.Sqrt(2000.0/1999.0), 1e-9*amp)
	}
}

// The mean of a ramp 0..n-1 is (n-1)/2 and its integral by the trapezoid rule is
// exact: the area of a line from 0 to n-1 over n-1 intervals of dt is (n-1)^2*dt/2.
func TestIntegralOfARampAndASine(t *testing.T) {
	n := 101
	ramp := make([]float64, n)
	for i := range ramp {
		ramp[i] = float64(i)
	}
	m, _ := Mean(ramp)
	near(t, "ramp mean", m, 50, 0)
	a, _ := Integrate(ramp, 0.01)
	near(t, "ramp integral", a, 100*100*0.01/2, 1e-12)
	run, _ := RunningIntegral(ramp, 0.01)
	near(t, "running first", run[0], 0, 0)
	near(t, "running last", run[n-1], a, 1e-12)
	near(t, "running at 10", run[10], 10*10*0.01/2, 1e-12)

	// One whole period of a sine integrates to zero. The trapezoid rule on a
	// periodic signal sampled over a whole period is exact up to rounding.
	s := sine(3, 5, 1000, 201) // 1000/5 = 200 samples per period; 201 points close the period
	area, _ := Integrate(s, 0.001)
	near(t, "sine integral over one period", area, 0, 1e-12)
	// Half a period of sin from 0 to pi integrates to 2 (closed form); trapezoid error is
	// h^2/12 * (f'(b)-f'(a)) = (pi/1000)^2/12 * 2, about 1.6e-6.
	h := make([]float64, 1001)
	for i := range h {
		h[i] = math.Sin(math.Pi * float64(i) / 1000)
	}
	half, _ := Integrate(h, math.Pi/1000)
	near(t, "half sine", half, 2, 2e-6)
}

// The derivative of 3t^2 is 6t. Central differences are exact on a parabola, and
// the ends use the one-sided slope, which is exact for a line only.
func TestDerivativeOfAParabola(t *testing.T) {
	dt := 0.1
	x := make([]float64, 11)
	for i := range x {
		tt := float64(i) * dt
		x[i] = 3 * tt * tt
	}
	d, err := Derivative(x, dt)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 10; i++ {
		near(t, "inside", d[i], 6*float64(i)*dt, 1e-12)
	}
	// One-sided at the first sample: (3*0.01 - 0)/0.1 = 0.3.
	near(t, "first", d[0], 0.3, 1e-12)
	near(t, "last", d[10], (3*1.0-3*0.81)/0.1, 1e-12)
	// And the derivative of a line is its slope everywhere, ends included.
	line := []float64{1, 3, 5, 7}
	dl, _ := Derivative(line, 0.5)
	for i, v := range dl {
		near(t, "line", v, 4, 1e-12)
		_ = i
	}
}

func TestBadInputIsAnErrorNotAPanic(t *testing.T) {
	nan := math.NaN()
	for name, fn := range map[string]func() error{
		"mean empty":     func() error { _, e := Mean(nil); return e },
		"rms empty":      func() error { _, e := RMS([]float64{}); return e },
		"min empty":      func() error { _, e := Min(nil); return e },
		"max empty":      func() error { _, e := Max(nil); return e },
		"stddev one":     func() error { _, e := StdDev([]float64{1}); return e },
		"mean nan":       func() error { _, e := Mean([]float64{1, nan}); return e },
		"max inf":        func() error { _, e := Max([]float64{1, math.Inf(1)}); return e },
		"integral dt0":   func() error { _, e := Integrate([]float64{1, 2}, 0); return e },
		"integral empty": func() error { _, e := Integrate(nil, 1); return e },
		"running dt neg": func() error { _, e := RunningIntegral([]float64{1, 2}, -1); return e },
		"deriv one":      func() error { _, e := Derivative([]float64{1}, 1); return e },
		"deriv dt nan":   func() error { _, e := Derivative([]float64{1, 2}, nan); return e },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Mean(nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty error reads %v", err)
	}
	a, err := Integrate([]float64{5}, 1)
	if err != nil || a != 0 {
		t.Errorf("one sample integrates to %v, %v", a, err)
	}
}

// Small readings on a large offset keep their spread: 1e9 plus 0.1, 0.2, 0.3.
func TestStdDevOnALargeOffset(t *testing.T) {
	sd, _ := StdDev([]float64{1e9 + 0.1, 1e9 + 0.2, 1e9 + 0.3})
	near(t, "stddev", sd, 0.1, 1e-6)
}
