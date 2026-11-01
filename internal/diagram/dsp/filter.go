package dsp

import (
	"fmt"
	"math"
	"math/cmplx"
)

// Kind is the shape of a filter.
type Kind string

const (
	Lowpass  Kind = "lowpass"
	Highpass Kind = "highpass"
	Bandpass Kind = "bandpass"
	Bandstop Kind = "bandstop"
)

// MaxOrder is the highest order Design accepts. For a band kind the order is that
// of the lowpass it is made from, so the filter has twice as many poles.
const MaxOrder = 10

// Section is one second-order stage, y[n] = b0 x[n] + b1 x[n-1] + b2 x[n-2]
// - a1 y[n-1] - a2 y[n-2]. A first-order stage has b2 and a2 of zero.
type Section struct{ B0, B1, B2, A1, A2 float64 }

// Design makes a Butterworth filter of the given order for a sample rate in hertz.
// Lowpass and highpass take one cutoff, the -3 dB point, in f1. Bandpass and
// bandstop take their two -3 dB edges, f1 below f2. It is the analog Butterworth
// prototype moved to the chosen frequencies and turned into a digital filter by
// the bilinear transform, with the edges prewarped so the -3 dB points land on the
// frequencies asked for. The result is a cascade of second-order sections, which
// stays accurate at orders where one long polynomial would not, and it is checked
// to be stable before it is returned.
func Design(kind Kind, order int, rate, f1, f2 float64) ([]Section, error) {
	if order < 1 || order > MaxOrder {
		return nil, fmt.Errorf("the order must be from 1 to %d, got %d", MaxOrder, order)
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, fmt.Errorf("the sample rate must be above zero, got %v", rate)
	}
	nyq := rate / 2
	edge := func(name string, f float64) error {
		if math.IsNaN(f) || f <= 0 {
			return fmt.Errorf("%s must be above zero, got %v", name, f)
		}
		if f >= nyq {
			return fmt.Errorf("%s must be below half the sample rate (%v Hz), got %v", name, nyq, f)
		}
		return nil
	}
	switch kind {
	case Lowpass, Highpass:
		if err := edge("the cutoff", f1); err != nil {
			return nil, err
		}
	case Bandpass, Bandstop:
		if err := edge("the lower edge", f1); err != nil {
			return nil, err
		}
		if err := edge("the upper edge", f2); err != nil {
			return nil, err
		}
		if f1 >= f2 {
			return nil, fmt.Errorf("the lower edge (%v Hz) must be below the upper edge (%v Hz)", f1, f2)
		}
	default:
		return nil, fmt.Errorf("%q is not a filter kind (lowpass, highpass, bandpass, bandstop)", kind)
	}

	// Analog Butterworth poles on the left half of the unit circle.
	proto := make([]complex128, order)
	for k := 0; k < order; k++ {
		th := math.Pi * float64(2*k+order+1) / float64(2*order)
		proto[k] = cmplx.Rect(1, th)
	}

	fs2 := 2 * rate
	warp := func(f float64) float64 {
		if !prewarp {
			return 2 * math.Pi * f
		}
		return fs2 * math.Tan(math.Pi*f/rate)
	}

	var poles []complex128
	var w0 float64 // the center, in the digital domain, for a band kind
	switch kind {
	case Lowpass:
		wc := warp(f1)
		for _, p := range proto {
			poles = append(poles, p*complex(wc, 0))
		}
	case Highpass:
		wc := warp(f1)
		for _, p := range proto {
			poles = append(poles, complex(wc, 0)/p)
		}
	case Bandpass, Bandstop:
		wl, wh := warp(f1), warp(f2)
		bw, wo := wh-wl, math.Sqrt(wl*wh)
		w0 = 2 * math.Atan(wo/fs2)
		for _, p := range proto {
			var q complex128
			if kind == Bandpass {
				q = p * complex(bw/2, 0)
			} else {
				q = complex(bw/2, 0) / p
			}
			d := cmplx.Sqrt(q*q - complex(wo*wo, 0))
			poles = append(poles, q+d, q-d)
		}
	}
	zp := make([]complex128, len(poles))
	for i, s := range poles {
		zp[i] = (complex(fs2, 0) + s) / (complex(fs2, 0) - s)
	}

	secs := pair(zp, kind, w0)
	// Normalize each section to unit gain where its filter passes.
	ref := 0.0
	switch kind {
	case Highpass:
		ref = math.Pi
	case Bandpass:
		ref = w0
	}
	for i := range secs {
		g := secs[i].mag(ref)
		if g == 0 || math.IsNaN(g) || math.IsInf(g, 0) {
			return nil, fmt.Errorf("the design is not usable at this cutoff and sample rate")
		}
		secs[i].B0, secs[i].B1, secs[i].B2 = secs[i].B0/g, secs[i].B1/g, secs[i].B2/g
	}
	if !Stable(secs) {
		return nil, fmt.Errorf("this design is not stable at this cutoff and sample rate; use a lower order or a cutoff further from 0 and half the sample rate")
	}
	return secs, nil
}

// prewarp is on. The tests turn it off to show the 40 dB check depends on it.
var prewarp = true

// pair groups the digital poles into second-order sections and gives each the
// zeros its kind calls for: lowpass at -1, highpass at +1, bandpass one at each,
// and bandstop a conjugate pair on the unit circle at the center w0.
func pair(zp []complex128, kind Kind, w0 float64) []Section {
	var cplx []complex128 // one of each conjugate pair, imaginary part above zero
	var reals []float64
	for _, p := range zp {
		switch {
		case math.Abs(imag(p)) < 1e-9*math.Max(1, cmplx.Abs(p)):
			reals = append(reals, real(p))
		case imag(p) > 0:
			cplx = append(cplx, p)
		}
	}
	var out []Section
	num := func(order2 bool) (b0, b1, b2 float64) {
		switch kind {
		case Lowpass:
			if order2 {
				return 1, 2, 1
			}
			return 1, 1, 0
		case Highpass:
			if order2 {
				return 1, -2, 1
			}
			return 1, -1, 0
		case Bandpass:
			return 1, 0, -1
		}
		return 1, -2 * math.Cos(w0), 1 // bandstop
	}
	for _, p := range cplx {
		b0, b1, b2 := num(true)
		out = append(out, Section{b0, b1, b2, -2 * real(p), real(p)*real(p) + imag(p)*imag(p)})
	}
	for len(reals) >= 2 {
		p1, p2 := reals[0], reals[1]
		reals = reals[2:]
		b0, b1, b2 := num(true)
		out = append(out, Section{b0, b1, b2, -(p1 + p2), p1 * p2})
	}
	if len(reals) == 1 {
		b0, b1, b2 := num(false)
		out = append(out, Section{b0, b1, b2, -reals[0], 0})
	}
	return out
}

// mag is |H(e^jw)| of one section.
func (s Section) mag(w float64) float64 {
	z1 := cmplx.Rect(1, -w)
	z2 := z1 * z1
	num := complex(s.B0, 0) + complex(s.B1, 0)*z1 + complex(s.B2, 0)*z2
	den := 1 + complex(s.A1, 0)*z1 + complex(s.A2, 0)*z2
	return cmplx.Abs(num / den)
}

// Stable reports whether every section has both poles inside the unit circle.
func Stable(secs []Section) bool {
	for _, s := range secs {
		if !(math.Abs(s.A2) < 1 && math.Abs(s.A1) < 1+s.A2) {
			return false
		}
	}
	return len(secs) > 0
}

// Stream filters a signal that arrives in chunks and keeps each section's memory
// between calls, so cutting a signal in pieces gives the same samples as filtering
// it whole. It starts from rest.
type Stream struct {
	secs []Section
	z    [][2]float64
}

// NewStream starts a filter from rest.
func NewStream(secs []Section) *Stream {
	return &Stream{secs: secs, z: make([][2]float64, len(secs))}
}

// Process filters one chunk and returns a new slice.
func (f *Stream) Process(x []float64) []float64 {
	y := make([]float64, len(x))
	copy(y, x)
	for k, s := range f.secs {
		z := &f.z[k]
		for i, v := range y {
			o := s.B0*v + z[0]
			z[0] = s.B1*v - s.A1*o + z[1]
			z[1] = s.B2*v - s.A2*o
			y[i] = o
		}
	}
	return y
}

// Filter runs a whole signal through the sections from rest.
func Filter(x []float64, secs []Section) ([]float64, error) {
	if err := check(x, 1); err != nil {
		return nil, err
	}
	if !Stable(secs) {
		return nil, fmt.Errorf("the filter is empty or not stable")
	}
	return NewStream(secs).Process(x), nil
}

// Response is the filter's gain in decibels at each frequency in hertz, for a
// signal sampled at rate. A frequency outside 0 to half the rate is an error. A
// gain of exactly zero, such as a notch's center, reads as -300 dB.
func Response(secs []Section, rate float64, freqs []float64) ([]float64, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, fmt.Errorf("the sample rate must be above zero, got %v", rate)
	}
	out := make([]float64, len(freqs))
	for i, f := range freqs {
		if math.IsNaN(f) || f < 0 || f > rate/2 {
			return nil, fmt.Errorf("frequency %v Hz is outside 0 to %v Hz", f, rate/2)
		}
		g := 1.0
		for _, s := range secs {
			g *= s.mag(2 * math.Pi * f / rate)
		}
		out[i] = math.Max(20*math.Log10(g), -300)
	}
	return out, nil
}
