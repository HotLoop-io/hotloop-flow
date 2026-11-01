package ni

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Task is one DAQmx task on the server: channels, a sample clock and optionally
// a start trigger, then reads. Make it with NewTask and always Clear it.
type Task struct {
	c        *Client
	session  string
	channels int
	rate     float64
	cleared  bool
}

// NewTask creates an empty task on the server.
func (c *Client) NewTask(ctx context.Context) (*Task, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	out, err := c.call(ctx, "CreateTask", encCreateTask("flow-"+hex.EncodeToString(id[:])), 0)
	if err != nil {
		return nil, err
	}
	_, name, err := decStatus(out)
	if err != nil {
		return nil, &Error{Method: "CreateTask", Message: "the answer is not the message this call expects: " + err.Error()}
	}
	if name == "" {
		return nil, &Error{Method: "CreateTask", Message: "the server made a task and named no session for it"}
	}
	return &Task{c: c, session: name}, nil
}

// Terminal is how an analog input is wired.
type Terminal string

const (
	TerminalDefault    Terminal = "default"
	TerminalRSE        Terminal = "rse"
	TerminalNRSE       Terminal = "nrse"
	TerminalDifferent  Terminal = "differential"
	TerminalPseudoDiff Terminal = "pseudodifferential"
)

func (t Terminal) enum() (int32, error) {
	switch strings.ToLower(string(t)) {
	case "", "default":
		return termDefault, nil
	case "rse":
		return termRSE, nil
	case "nrse":
		return termNRSE, nil
	case "differential", "diff":
		return termDiff, nil
	case "pseudodifferential", "pseudodiff":
		return termPseudoDiff, nil
	}
	return 0, fmt.Errorf("ni: %q is not a terminal configuration (default, rse, nrse, differential, pseudodifferential)", string(t))
}

// Channel is an analog input channel. Physical is NI's name for it, such as
// "cDAQ1Mod1/ai0" or the range "cDAQ1Mod1/ai0:3". Min and Max are the expected
// range of the signal in the channel's units.
type Channel struct {
	Physical string
	Name     string
	Terminal Terminal
	Min, Max float64
}

func (t *Task) common(ch Channel) (aiCommon, int, error) {
	term, err := ch.Terminal.enum()
	if err != nil {
		return aiCommon{}, 0, err
	}
	n, err := channelCount(ch.Physical)
	if err != nil {
		return aiCommon{}, 0, err
	}
	if ch.Min == 0 && ch.Max == 0 {
		return aiCommon{}, 0, errors.New("ni: a channel needs the range of its signal, a min and a max")
	}
	return aiCommon{task: t.session, physical: ch.Physical, name: ch.Name, terminal: term, min: ch.Min, max: ch.Max}, n, nil
}

func (t *Task) add(ctx context.Context, method string, req []byte, n int) error {
	if _, err := t.c.call(ctx, method, req, 0); err != nil {
		return err
	}
	t.channels += n
	return nil
}

// AddVoltage adds analog voltage input channels, in volts.
func (t *Task) AddVoltage(ctx context.Context, ch Channel) error {
	c, n, err := t.common(ch)
	if err != nil {
		return err
	}
	return t.add(ctx, "CreateAIVoltageChan", encVoltage(c), n)
}

// AddCurrent adds analog current input channels, in amps, read across the
// module's own shunt resistor.
func (t *Task) AddCurrent(ctx context.Context, ch Channel) error {
	c, n, err := t.common(ch)
	if err != nil {
		return err
	}
	return t.add(ctx, "CreateAICurrentChan", encCurrent(c), n)
}

// Thermocouple adds temperature input channels read from thermocouples, with
// the module's built in cold junction compensation.
type Thermocouple struct {
	Channel
	// Type is one letter, B, E, J, K, N, R, S or T.
	Type string
	// Unit is "C", "F", "K" or "R". Empty means C.
	Unit string
}

// AddThermocouple adds temperature input channels.
func (t *Task) AddThermocouple(ctx context.Context, tc Thermocouple) error {
	typ, ok := map[string]int32{"B": tcB, "E": tcE, "J": tcJ, "K": tcK, "N": tcN, "R": tcR, "S": tcS, "T": tcT}[strings.ToUpper(tc.Type)]
	if !ok {
		return fmt.Errorf("ni: %q is not a thermocouple type (B, E, J, K, N, R, S, T)", tc.Type)
	}
	unit, ok := map[string]int32{"": tempC, "C": tempC, "F": tempF, "K": tempK, "R": tempR}[strings.ToUpper(tc.Unit)]
	if !ok {
		return fmt.Errorf("ni: %q is not a temperature unit (C, F, K, R)", tc.Unit)
	}
	c, n, err := t.common(tc.Channel)
	if err != nil {
		return err
	}
	return t.add(ctx, "CreateAIThrmcplChan", encThermocouple(c, unit, typ), n)
}

// Mode is how long the clock runs.
type Mode string

const (
	// Finite takes SamplesPerChannel samples and stops.
	Finite Mode = "finite"
	// Continuous runs until the task is stopped; SamplesPerChannel sizes the buffer.
	Continuous Mode = "continuous"
)

// Clock sets the sample clock.
type Clock struct {
	// Source is the clock terminal. Empty means the device's own clock.
	Source string
	// Rate is samples per second per channel.
	Rate float64
	Mode Mode
	// SamplesPerChannel is how many to take (finite) or how big a buffer to keep (continuous).
	SamplesPerChannel uint64
	// Falling samples on the falling edge instead of the rising one.
	Falling bool
}

// SetClock configures the sample clock. Do it after the channels are added.
func (t *Task) SetClock(ctx context.Context, clk Clock) error {
	if !(clk.Rate > 0) {
		return fmt.Errorf("ni: the sample rate must be above zero, not %v", clk.Rate)
	}
	mode := int32(acquisitionFinite)
	switch clk.Mode {
	case Finite, "":
	case Continuous:
		mode = acquisitionContinuous
	default:
		return fmt.Errorf("ni: %q is not a mode (finite, continuous)", string(clk.Mode))
	}
	if _, err := t.c.call(ctx, "CfgSampClkTiming", encSampClk(t.session, clk.Source, clk.Rate, mode, clk.Falling, clk.SamplesPerChannel), 0); err != nil {
		return err
	}
	t.rate = clk.Rate
	return nil
}

// StartOnEdge makes the task wait for a digital edge on a terminal, such as
// "/cDAQ1/PFI0", before it starts sampling.
func (t *Task) StartOnEdge(ctx context.Context, terminal string, falling bool) error {
	_, err := t.c.call(ctx, "CfgDigEdgeStartTrig", encStartTrig(t.session, terminal, falling), 0)
	return err
}

// Start starts the task.
func (t *Task) Start(ctx context.Context) error {
	_, err := t.c.call(ctx, "StartTask", encTask(t.session), 0)
	return err
}

// Stop stops the task and leaves it configured, ready to start again.
func (t *Task) Stop(ctx context.Context) error {
	_, err := t.c.call(ctx, "StopTask", encTask(t.session), 0)
	return err
}

// Clear stops the task if it is running and frees it on the server. A task
// that was cleared clears again without error.
func (t *Task) Clear(ctx context.Context) error {
	if t.cleared {
		return nil
	}
	if _, err := t.c.call(ctx, "ClearTask", encTask(t.session), 0); err != nil {
		return err
	}
	t.cleared = true
	return nil
}

// Samples is what a read returns: one slice of values per channel, in the order
// the channels were added, and the time between samples.
type Samples struct {
	// Data[c][i] is channel c's sample i.
	Data [][]float64
	// Dt is the time between samples in seconds, from the sample clock's rate.
	Dt float64
	// Warning is a positive DAQmx status the read came back with, or zero.
	Warning int32
}

// Span is how long the samples cover, in seconds: the count times the time
// between samples.
func (s Samples) Span() float64 {
	if len(s.Data) == 0 {
		return 0
	}
	return float64(len(s.Data[0])) * s.Dt
}

// Read takes n samples from every channel. timeout is how long the driver may
// wait for them, in seconds; the call itself is allowed that long plus the
// client's call timeout. A short read is an error: the caller gets exactly n
// samples per channel or nothing.
func (t *Task) Read(ctx context.Context, n int, timeout float64) (Samples, error) {
	if n <= 0 {
		return Samples{}, fmt.Errorf("ni: cannot read %d samples", n)
	}
	if t.channels == 0 {
		return Samples{}, errors.New("ni: the task has no channels to read")
	}
	total := n * t.channels
	if total > math.MaxInt32 {
		return Samples{}, fmt.Errorf("ni: %d samples on %d channels is more than one read can carry", n, t.channels)
	}
	out, err := t.c.call(ctx, "ReadAnalogF64", encRead(t.session, int32(n), timeout, uint32(total)), durationOf(timeout))
	if err != nil {
		return Samples{}, err
	}
	r, err := decRead(out)
	if err != nil {
		return Samples{}, &Error{Method: "ReadAnalogF64", Message: err.Error()}
	}
	if int(r.perChan) != n || len(r.values) < total {
		return Samples{}, &Error{Method: "ReadAnalogF64", Message: fmt.Sprintf("asked for %d samples on each of %d channels and the server returned %d per channel (%d values)", n, t.channels, r.perChan, len(r.values))}
	}
	s := Samples{Data: make([][]float64, t.channels), Warning: r.status}
	for c := range s.Data {
		s.Data[c] = append([]float64(nil), r.values[c*n:(c+1)*n]...)
	}
	if t.rate > 0 {
		s.Dt = 1 / t.rate
	}
	return s, nil
}

// Channels is how many channels the task has so far.
func (t *Task) Channels() int { return t.channels }

// channelCount says how many channels a physical channel string names: one
// name is one, "dev/ai0:3" is four, and a comma list adds them up.
func channelCount(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("ni: a channel needs a physical channel name such as cDAQ1Mod1/ai0")
	}
	total := 0
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("ni: %q has an empty channel in it", s)
		}
		i := strings.LastIndex(part, ":")
		if i < 0 {
			total++
			continue
		}
		lo, hi := part[:i], part[i+1:]
		j := len(lo)
		for j > 0 && lo[j-1] >= '0' && lo[j-1] <= '9' {
			j--
		}
		a, err1 := strconv.Atoi(lo[j:])
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("ni: %q is not a channel range like cDAQ1Mod1/ai0:3", part)
		}
		if b < a {
			a, b = b, a
		}
		total += b - a + 1
	}
	return total, nil
}
