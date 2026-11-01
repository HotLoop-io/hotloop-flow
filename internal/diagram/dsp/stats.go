// Package dsp is the arithmetic behind FlowLab's analysis nodes: statistics of an
// array, integral and derivative, and Butterworth filters. It is plain Go on
// slices of float64, imports nothing from the rest of Flow, and returns an error
// instead of panicking on input it cannot use, so it can be fuzzed and reused.
package dsp

import (
	"errors"
	"fmt"
	"math"
)

// ErrEmpty is returned when a calculation needs at least one sample and got none.
var ErrEmpty = errors.New("the array is empty")

// check refuses an empty array and any sample that is not a finite number, with
// the position of the first bad one.
func check(x []float64, min int) error {
	if len(x) == 0 {
		return ErrEmpty
	}
	if len(x) < min {
		return fmt.Errorf("needs at least %d samples, got %d", min, len(x))
	}
	for i, v := range x {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("sample %d is not a finite number", i)
		}
	}
	return nil
}

// sum adds with Neumaier compensation, so a long array of small readings next to a
// large offset does not lose its small digits.
func sum(n int, at func(i int) float64) float64 {
	var s, c float64
	for i := 0; i < n; i++ {
		v := at(i)
		t := s + v
		if math.Abs(s) >= math.Abs(v) {
			c += (s - t) + v
		} else {
			c += (v - t) + s
		}
		s = t
	}
	return s + c
}

// Mean is the arithmetic mean.
func Mean(x []float64) (float64, error) {
	if err := check(x, 1); err != nil {
		return 0, err
	}
	return sum(len(x), func(i int) float64 { return x[i] }) / float64(len(x)), nil
}

// RMS is the root mean square, sqrt(mean(x^2)). For a sine of amplitude A over a
// whole number of periods it is A/sqrt(2).
func RMS(x []float64) (float64, error) {
	if err := check(x, 1); err != nil {
		return 0, err
	}
	return math.Sqrt(sum(len(x), func(i int) float64 { return x[i] * x[i] }) / float64(len(x))), nil
}

// Min is the smallest sample.
func Min(x []float64) (float64, error) {
	if err := check(x, 1); err != nil {
		return 0, err
	}
	m := x[0]
	for _, v := range x[1:] {
		if v < m {
			m = v
		}
	}
	return m, nil
}

// Max is the largest sample.
func Max(x []float64) (float64, error) {
	if err := check(x, 1); err != nil {
		return 0, err
	}
	m := x[0]
	for _, v := range x[1:] {
		if v > m {
			m = v
		}
	}
	return m, nil
}

// StdDev is the sample standard deviation, which divides by n-1, not n. A test
// bench measures a few readings of a larger population, and this is the form a
// test engineer expects from a spreadsheet's STDEV. It needs two samples, because
// one reading has no spread to estimate. It is the two-pass form, which stays
// accurate when the readings sit on a large offset.
func StdDev(x []float64) (float64, error) {
	if err := check(x, 2); err != nil {
		return 0, err
	}
	m, _ := Mean(x)
	ss := sum(len(x), func(i int) float64 { d := x[i] - m; return d * d })
	return math.Sqrt(ss / float64(len(x)-1)), nil
}

// Integrate is the area under the samples by the trapezoid rule, with dt seconds
// between samples. One sample has no width, so its area is zero.
func Integrate(x []float64, dt float64) (float64, error) {
	if err := checkDT(dt); err != nil {
		return 0, err
	}
	if err := check(x, 1); err != nil {
		return 0, err
	}
	return sum(len(x)-1, func(i int) float64 { return (x[i] + x[i+1]) / 2 * dt }), nil
}

// RunningIntegral is the trapezoid area up to each sample: the first is zero and
// the last equals Integrate.
func RunningIntegral(x []float64, dt float64) ([]float64, error) {
	if err := checkDT(dt); err != nil {
		return nil, err
	}
	if err := check(x, 1); err != nil {
		return nil, err
	}
	out := make([]float64, len(x))
	for i := 1; i < len(x); i++ {
		out[i] = out[i-1] + (x[i-1]+x[i])/2*dt
	}
	return out, nil
}

// Derivative is the slope at each sample, with dt seconds between samples. It uses
// central differences inside the array, which is exact for a parabola, and a
// one-sided difference at the first and last sample.
func Derivative(x []float64, dt float64) ([]float64, error) {
	if err := checkDT(dt); err != nil {
		return nil, err
	}
	if err := check(x, 2); err != nil {
		return nil, err
	}
	n := len(x)
	out := make([]float64, n)
	out[0] = (x[1] - x[0]) / dt
	out[n-1] = (x[n-1] - x[n-2]) / dt
	for i := 1; i < n-1; i++ {
		out[i] = (x[i+1] - x[i-1]) / (2 * dt)
	}
	return out, nil
}

func checkDT(dt float64) error {
	if math.IsNaN(dt) || math.IsInf(dt, 0) || dt <= 0 {
		return fmt.Errorf("the time between samples must be above zero, got %v", dt)
	}
	return nil
}
