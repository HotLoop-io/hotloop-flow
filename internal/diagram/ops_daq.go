package diagram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/ni"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// daqSession is the DAQ task a run keeps for one dia-daq-read node. The first
// call makes it, every later call reads from it, and the end of the run clears it
// on the server.
type daqSession struct {
	mu      sync.Mutex
	client  *ni.Client
	task    *ni.Task
	started bool
}

// Close clears the task on the server, which frees the hardware, and drops the
// connection. A run calls it when it ends, whether it ended well or not.
func (s *daqSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *daqSession) closeLocked() error {
	var err error
	if s.task != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = s.task.Clear(ctx)
		cancel()
		s.task = nil
	}
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	s.started = false
	return err
}

type daqSettings struct {
	cfg        ni.Config
	kind       string
	physical   string
	channel    ni.Channel
	tcType     string
	unit       string
	clock      ni.Clock
	samples    int
	trigger    string
	trigFall   bool
	timeoutSec float64
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-daq-read", Color: "#c3d3d9", Icon: "daq", PaletteLabel: "read DAQ task", LabelProp: "channels",
			InputPorts: []string{"err?"}, OutputPorts: []string{"y", "dt", "err"},
			Props: []node.Prop{
				{Name: "address", Label: "Server address (host:port)", Kind: node.PropString, Required: true, Default: "localhost:31763",
					Help: "Where the NI gRPC Device Server listens. It listens on this machine only until its configuration file says otherwise."},
				choice("tls", "Security", "off", "off", "on", "mutual"),
				prop("caFile", "Authority certificate file (PEM) that signed the server's certificate", node.PropString, ""),
				prop("certFile", "Client certificate file (PEM), for mutual security", node.PropString, ""),
				prop("keyFile", "Client key file (PEM), for mutual security", node.PropString, ""),
				prop("serverName", "Name in the server's certificate, if it is not the host in the address", node.PropString, ""),
				{Name: "device", Label: "Device (such as cDAQ1Mod1)", Kind: node.PropString, Required: true, Default: "cDAQ1Mod1"},
				choice("kind", "Channel kind", "voltage", "voltage", "current", "thermocouple"),
				{Name: "channels", Label: "Channels (such as ai0:3, or ai0, ai2)", Kind: node.PropString, Required: true, Default: "ai0"},
				prop("min", "Smallest value expected", node.PropNumber, -10),
				prop("max", "Largest value expected", node.PropNumber, 10),
				choice("terminal", "Wiring", "default", "default", "rse", "nrse", "differential", "pseudodifferential"),
				choice("tcType", "Thermocouple type", "K", "B", "E", "J", "K", "N", "R", "S", "T"),
				choice("unit", "Temperature unit", "C", "C", "F", "K", "R"),
				choice("mode", "Mode", "finite", "finite", "continuous"),
				prop("rate", "Sample rate per channel (samples per second)", node.PropNumber, 1000),
				prop("samples", "Samples per channel to read", node.PropNumber, 1000),
				prop("trigger", "Start on a digital edge at this terminal (empty to start at once)", node.PropString, ""),
				choice("triggerEdge", "Start edge", "rising", "rising", "falling"),
				prop("callTimeoutSec", "Seconds to wait for the server to answer each call", node.PropNumber, 10),
				prop("timeoutSec", "Seconds the hardware may take to deliver the samples (empty for the acquisition time plus 10)", node.PropNumber, ""),
			},
			Help: "Reads samples from a data acquisition task on NI hardware, through NI's gRPC Device Server. " +
				"The host the hardware is attached to needs NI's DAQmx driver and the NI gRPC Device Server, started with a configuration that turns on TLS, and with an address other than localhost if Flow runs on another machine. " +
				"Never leave the server's unsecured port open on a plant network: set Security to on, or to mutual to also check this node's own certificate. " +
				"Finite mode takes one acquisition of the samples you set on every call. Continuous mode starts the task on the first call and reads the next block of samples on each call after it. " +
				"y is the samples of one channel as an array, or an array of arrays, one per channel in order. dt is the seconds between samples. " +
				"A refusal from the server or the driver is an error on the error wire with the driver's own code and text. The task is cleared on the server when the run ends. " +
				"This node has been tested against a stand-in server only, not against NI's server.",
		},
		New: func(raw map[string]any) (Op, error) {
			set, err := daqFromProps(raw)
			if err != nil {
				return nil, err
			}
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				s := rc.Run.Local("daq:"+rc.Node.ID, func() any { return &daqSession{} }).(*daqSession)
				return s.read(ctx, set)
			}), nil
		},
	})
}

func daqFromProps(raw map[string]any) (*daqSettings, error) {
	set := &daqSettings{}
	set.cfg.Address = strProp(raw, "address", "localhost:31763")
	if ct := floatProp(raw, "callTimeoutSec", 10); ct > 0 {
		set.cfg.CallTimeout = time.Duration(ct * float64(time.Second))
	}
	switch strProp(raw, "tls", "off") {
	case "off":
	case "on":
		set.cfg.TLS = ni.TLS{Enabled: true, RootCAFile: strProp(raw, "caFile", ""), ServerName: strProp(raw, "serverName", "")}
	case "mutual":
		set.cfg.TLS = ni.TLS{Enabled: true, RootCAFile: strProp(raw, "caFile", ""), ServerName: strProp(raw, "serverName", ""),
			CertFile: strProp(raw, "certFile", ""), KeyFile: strProp(raw, "keyFile", "")}
		if set.cfg.TLS.CertFile == "" || set.cfg.TLS.KeyFile == "" {
			return nil, errors.New("mutual security needs this node's own certificate file and key file")
		}
	default:
		return nil, fmt.Errorf("%q is not a security setting (off, on, mutual)", strProp(raw, "tls", ""))
	}
	if _, err := ni.Dial(ni.Config{Address: set.cfg.Address}); err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "ni: "))
	}
	device := strings.Trim(strProp(raw, "device", ""), "/ ")
	if device == "" {
		return nil, errors.New("the DAQ read has no device")
	}
	chans := splitList(strProp(raw, "channels", ""))
	if len(chans) == 0 {
		return nil, errors.New("the DAQ read has no channels")
	}
	phys := make([]string, len(chans))
	for i, c := range chans {
		phys[i] = device + "/" + strings.TrimLeft(c, "/")
	}
	set.physical = strings.Join(phys, ", ")
	set.kind = strProp(raw, "kind", "voltage")
	set.channel = ni.Channel{Physical: set.physical, Terminal: ni.Terminal(strProp(raw, "terminal", "default")),
		Min: floatProp(raw, "min", -10), Max: floatProp(raw, "max", 10)}
	if !(set.channel.Min < set.channel.Max) {
		return nil, fmt.Errorf("the smallest value %v is not below the largest %v", set.channel.Min, set.channel.Max)
	}
	switch set.kind {
	case "voltage", "current":
	case "thermocouple":
		set.tcType, set.unit = strProp(raw, "tcType", "K"), strProp(raw, "unit", "C")
	default:
		return nil, fmt.Errorf("%q is not a channel kind (voltage, current, thermocouple)", set.kind)
	}
	rate := floatProp(raw, "rate", 1000)
	if !(rate > 0) {
		return nil, fmt.Errorf("the sample rate must be above zero, not %v", rate)
	}
	set.samples = intProp(raw, "samples", 1000)
	set.clock = ni.Clock{Rate: rate, Mode: ni.Mode(strProp(raw, "mode", "finite")), SamplesPerChannel: uint64(set.samples)}
	if set.clock.Mode != ni.Finite && set.clock.Mode != ni.Continuous {
		return nil, fmt.Errorf("%q is not a mode (finite, continuous)", set.clock.Mode)
	}
	if set.clock.Mode == ni.Continuous && set.clock.SamplesPerChannel < 2 {
		set.clock.SamplesPerChannel = 2
	}
	set.trigger = strProp(raw, "trigger", "")
	set.trigFall = strProp(raw, "triggerEdge", "rising") == "falling"
	set.timeoutSec = floatProp(raw, "timeoutSec", 0)
	if set.timeoutSec <= 0 {
		set.timeoutSec = float64(set.samples)/rate + 10
	}
	// The terminal, the thermocouple type and the unit are checked here too, so a
	// wrong one is in the error list and not a failure in the middle of a run.
	if _, err := ni.CheckTerminal(set.channel.Terminal); err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "ni: "))
	}
	if set.kind == "thermocouple" {
		if err := ni.CheckThermocouple(set.tcType, set.unit); err != nil {
			return nil, errors.New(strings.TrimPrefix(err.Error(), "ni: "))
		}
	}
	return set, nil
}

// read makes the task if the run has none yet, and takes one block of samples.
func (s *daqSession) read(ctx context.Context, set *daqSettings) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == nil {
		if err := s.setup(ctx, set); err != nil {
			s.closeLocked()
			return nil, daqFault(err)
		}
	}
	if set.clock.Mode == ni.Finite && !s.started {
		if err := s.task.Start(ctx); err != nil {
			return nil, daqFault(err)
		}
		s.started = true
	}
	got, err := s.task.Read(ctx, set.samples, set.timeoutSec)
	if set.clock.Mode == ni.Finite {
		// One acquisition per call: stop so the next call can start it again.
		if serr := s.task.Stop(stopCtx(ctx)); serr != nil && err == nil {
			err = serr
		}
		s.started = false
	}
	if err != nil {
		return nil, daqFault(err)
	}
	return map[string]any{"y": daqShape(got.Data), "dt": got.Dt}, nil
}

func stopCtx(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return context.Background()
	}
	return ctx
}

func (s *daqSession) setup(ctx context.Context, set *daqSettings) error {
	c, err := ni.Dial(set.cfg)
	if err != nil {
		return err
	}
	s.client = c
	t, err := c.NewTask(ctx)
	if err != nil {
		return err
	}
	s.task = t
	switch set.kind {
	case "current":
		err = t.AddCurrent(ctx, set.channel)
	case "thermocouple":
		err = t.AddThermocouple(ctx, ni.Thermocouple{Channel: set.channel, Type: set.tcType, Unit: set.unit})
	default:
		err = t.AddVoltage(ctx, set.channel)
	}
	if err != nil {
		return err
	}
	if err = t.SetClock(ctx, set.clock); err != nil {
		return err
	}
	if set.trigger != "" {
		if err = t.StartOnEdge(ctx, set.trigger, set.trigFall); err != nil {
			return err
		}
	}
	if set.clock.Mode == ni.Continuous {
		if err = t.Start(ctx); err != nil {
			return err
		}
		s.started = true
	}
	return nil
}

// daqShape is one channel's samples as an array, or an array of arrays.
func daqShape(data [][]float64) any {
	conv := func(v []float64) []any {
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out
	}
	if len(data) == 1 {
		return conv(data[0])
	}
	out := make([]any, len(data))
	for i, ch := range data {
		out[i] = conv(ch)
	}
	return out
}

// daqFault turns what the client returned into the fault the error wire carries:
// the driver's own code and text when it gave them, a timeout code when the
// server did not answer in time.
func daqFault(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var ne *ni.Error
	if errors.As(err, &ne) {
		code := CodeDevice
		if ne.Timeout() {
			code = CodeTimeout
		}
		src := ne.Message
		if ne.Code != 0 {
			src = fmt.Sprintf("NI error %d: %s", ne.Code, ne.Message)
		}
		return &Fault{Code: code, Source: fmt.Sprintf("%s (%s)", src, ne.Method)}
	}
	return &Fault{Code: CodeDevice, Source: strings.TrimPrefix(err.Error(), "ni: ")}
}
