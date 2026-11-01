package ni

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/ni/nitest"
)

// Every test here runs against nitest's stand-in server, which is written by us
// and speaks the calls the client makes. None of it has run against NI's server.

func dial(t *testing.T, s *nitest.StandInServer, cfg Config) *Client {
	t.Helper()
	cfg.Address = s.Addr
	c, err := Dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStandInThousandSamplesSpanOneSecond(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	c := dial(t, s, Config{})
	ctx := context.Background()
	task, err := c.NewTask(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.AddVoltage(ctx, Channel{Physical: "cDAQ1Mod1/ai0:1", Terminal: "differential", Min: -10, Max: 10}); err != nil {
		t.Fatal(err)
	}
	if task.Channels() != 2 {
		t.Fatalf("channels %d, want 2", task.Channels())
	}
	if err := task.SetClock(ctx, Clock{Rate: 1000, Mode: Finite, SamplesPerChannel: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := task.Read(ctx, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 2 || len(got.Data[0]) != 1000 || len(got.Data[1]) != 1000 {
		t.Fatalf("shape %d channels, first has %d", len(got.Data), len(got.Data[0]))
	}
	if span := got.Span(); math.Abs(span-1.000) > 1e-9 {
		t.Fatalf("1,000 samples at 1 kS/s span %.9f s, want 1.000 s", span)
	}
	if math.Abs(got.Dt-0.001) > 1e-12 {
		t.Fatalf("dt %v, want 0.001", got.Dt)
	}
	for ch := 0; ch < 2; ch++ {
		for i := 0; i < 1000; i++ {
			if got.Data[ch][i] != nitest.Value(ch, int64(i), 1000) {
				t.Fatalf("channel %d sample %d is %v, want %v: the channels or the samples are out of order", ch, i, got.Data[ch][i], nitest.Value(ch, int64(i), 1000))
			}
		}
	}
	if err := task.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := task.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := task.Clear(ctx); err != nil {
		t.Fatalf("a second clear: %v", err)
	}
	if s.Tasks() != 0 {
		t.Fatalf("%d tasks left on the stand-in", s.Tasks())
	}
	want := []string{"CreateTask", "CreateAIVoltageChan", "CfgSampClkTiming", "StartTask", "ReadAnalogF64", "StopTask", "ClearTask"}
	if got := strings.Join(s.Calls(), ","); got != strings.Join(want, ",") {
		t.Fatalf("calls %s, want %s", got, strings.Join(want, ","))
	}
}

func TestStandInContinuousReadsPickUpWhereTheLastStopped(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	c := dial(t, s, Config{})
	ctx := context.Background()
	task, _ := c.NewTask(ctx)
	defer task.Clear(ctx)
	if err := task.AddVoltage(ctx, Channel{Physical: "cDAQ1Mod1/ai0", Min: -5, Max: 5}); err != nil {
		t.Fatal(err)
	}
	if err := task.SetClock(ctx, Clock{Rate: 2000, Mode: Continuous, SamplesPerChannel: 4000}); err != nil {
		t.Fatal(err)
	}
	if err := task.StartOnEdge(ctx, "/cDAQ1/PFI0", false); err != nil {
		t.Fatal(err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := task.Read(ctx, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := task.Read(ctx, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.Data[0][99] != nitest.Value(0, 99, 2000) || b.Data[0][0] != nitest.Value(0, 100, 2000) {
		t.Fatalf("the second read did not continue the first: %v then %v", a.Data[0][99], b.Data[0][0])
	}
}

func TestStandInCurrentAndThermocoupleChannels(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	c := dial(t, s, Config{})
	ctx := context.Background()
	task, _ := c.NewTask(ctx)
	defer task.Clear(ctx)
	if err := task.AddCurrent(ctx, Channel{Physical: "cDAQ1Mod1/ai0", Min: -0.02, Max: 0.02}); err != nil {
		t.Fatal(err)
	}
	if err := task.AddThermocouple(ctx, Thermocouple{Channel: Channel{Physical: "cDAQ1Mod1/ai1", Min: 0, Max: 300}, Type: "k", Unit: "F"}); err != nil {
		t.Fatal(err)
	}
	if err := task.AddThermocouple(ctx, Thermocouple{Channel: Channel{Physical: "cDAQ1Mod1/ai2", Min: 0, Max: 300}, Type: "Q"}); err == nil {
		t.Fatal("thermocouple type Q was accepted")
	}
	if task.Channels() != 2 {
		t.Fatalf("channels %d, want 2", task.Channels())
	}
}

func TestStandInErrorCarriesNICodeAndText(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	c := dial(t, s, Config{})
	ctx := context.Background()
	task, _ := c.NewTask(ctx)
	defer task.Clear(ctx)
	err := task.AddVoltage(ctx, Channel{Physical: "cDAQ9Mod9/ai0", Min: -10, Max: 10})
	var ne *Error
	if !errors.As(err, &ne) {
		t.Fatalf("error %v is not an *Error", err)
	}
	if ne.Code != -200220 {
		t.Fatalf("code %d, want -200220", ne.Code)
	}
	if ne.GRPC != 2 || ne.Method != "CreateAIVoltageChan" || !strings.Contains(ne.Message, "not in this chassis") {
		t.Fatalf("error %+v", ne)
	}
	if !strings.Contains(err.Error(), "-200220") || !strings.Contains(err.Error(), "not in this chassis") {
		t.Fatalf("the text %q hides the code or the reason", err.Error())
	}
	if task.Channels() != 0 {
		t.Fatalf("a refused channel was counted")
	}
	// A failure with no message ahead of it (trailers only) after a good one.
	s.FailNext("StartTask")
	_ = task.AddVoltage(ctx, Channel{Physical: "cDAQ1Mod1/ai0", Min: -1, Max: 1})
	_ = task.SetClock(ctx, Clock{Rate: 10, SamplesPerChannel: 10})
	err = task.Start(ctx)
	if !errors.As(err, &ne) || ne.Code != nitest.CodeStandInFault {
		t.Fatalf("start: %v", err)
	}
	if err := task.Start(ctx); err != nil {
		t.Fatalf("the next start: %v", err)
	}
}

func TestStandInRefusesWhatNIRefuses(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	c := dial(t, s, Config{})
	ctx := context.Background()
	task, _ := c.NewTask(ctx)
	defer task.Clear(ctx)
	if _, err := task.Read(ctx, 10, 1); err == nil {
		t.Fatal("a read with no channels")
	}
	if err := task.AddVoltage(ctx, Channel{Physical: "cDAQ1Mod1/ai0"}); err == nil {
		t.Fatal("a channel with no range")
	}
	if err := task.SetClock(ctx, Clock{Rate: 0}); err == nil {
		t.Fatal("a zero rate")
	}
	if err := task.SetClock(ctx, Clock{Rate: 10, Mode: "forever"}); err == nil {
		t.Fatal("an unknown mode")
	}
	if err := task.AddVoltage(ctx, Channel{Physical: "cDAQ1Mod1/ai0", Terminal: "sideways", Min: -1, Max: 1}); err == nil {
		t.Fatal("an unknown terminal")
	}
}

func TestServerThatNeverAnswersTimesOut(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	s.Hang(true)
	c := dial(t, s, Config{CallTimeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.NewTask(context.Background())
	var ne *Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("error %v is not a timeout", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 3*time.Second {
		t.Fatalf("gave up after %v, want about 200ms", d)
	}
}

func TestEveryCallHonorsItsContext(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	s.Hang(true)
	c := dial(t, s, Config{CallTimeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.NewTask(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not unwrap to a cancel", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v to notice the cancel", d)
	}
	// A deadline shorter than the call timeout wins.
	dl, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if _, err := c.NewTask(dl); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("deadline: %v after %v", err, time.Since(start))
	}
	// A read waits for the driver's timeout on top of the call timeout.
	s.Hang(false)
	c2 := dial(t, s, Config{CallTimeout: 100 * time.Millisecond})
	task, err := c2.NewTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer task.Clear(context.Background())
	_ = task.AddVoltage(context.Background(), Channel{Physical: "cDAQ1Mod1/ai0", Min: -1, Max: 1})
	_ = task.SetClock(context.Background(), Clock{Rate: 10, SamplesPerChannel: 10})
	s.Hang(true)
	start = time.Now()
	if _, err := task.Read(context.Background(), 10, 0.5); err == nil {
		t.Fatal("read answered")
	}
	if d := time.Since(start); d < 500*time.Millisecond || d > 4*time.Second {
		t.Fatalf("read gave up after %v, want about 0.6s (driver timeout plus call timeout)", d)
	}
}

func TestTLSAndMutualTLS(t *testing.T) {
	pki := nitest.NewPKI(t)
	sc, sk := pki.Issue(t, "server", true)
	cc, ck := pki.Issue(t, "client", false)
	ctx := context.Background()
	ok := func(t *testing.T, c *Client) {
		t.Helper()
		task, err := c.NewTask(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := task.Clear(ctx); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("server side TLS", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		c := dial(t, s, Config{TLS: TLS{RootCAFile: pki.CAFile}})
		ok(t, c)
		if s.PeerCerts() != 0 {
			t.Fatal("the server saw a client certificate nobody sent")
		}
	})
	t.Run("wrong authority is refused", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		other := nitest.NewPKI(t)
		c := dial(t, s, Config{TLS: TLS{RootCAFile: other.CAFile}})
		_, err := c.NewTask(ctx)
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("want a certificate error, got %v", err)
		}
	})
	t.Run("no authority given means the system's, which does not know ours", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		c := dial(t, s, Config{TLS: TLS{Enabled: true}})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a server certificate nobody vouched for was accepted")
		}
	})
	t.Run("the name is checked", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		c := dial(t, s, Config{TLS: TLS{RootCAFile: pki.CAFile, ServerName: "plant-daq.example"}})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a certificate for another name was accepted")
		}
	})
	t.Run("mutual TLS", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk, ClientCA: pki.CAFile})
		c := dial(t, s, Config{TLS: TLS{RootCAFile: pki.CAFile, CertFile: cc, KeyFile: ck}})
		ok(t, c)
		if s.PeerCerts() == 0 {
			t.Fatal("the server never saw the client certificate")
		}
	})
	t.Run("mutual TLS without a client certificate is refused", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk, ClientCA: pki.CAFile})
		c := dial(t, s, Config{TLS: TLS{RootCAFile: pki.CAFile}})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a client with no certificate got through mutual TLS")
		}
		if len(s.Calls()) != 0 {
			t.Fatalf("the server ran %v for an unauthenticated client", s.Calls())
		}
	})
	t.Run("a client certificate from another authority is refused", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk, ClientCA: pki.CAFile})
		other := nitest.NewPKI(t)
		oc, ok2 := other.Issue(t, "intruder", false)
		c := dial(t, s, Config{TLS: TLS{RootCAFile: pki.CAFile, CertFile: oc, KeyFile: ok2}})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a client certificate from the wrong authority got through")
		}
		if len(s.Calls()) != 0 {
			t.Fatalf("the server ran %v", s.Calls())
		}
	})
	t.Run("plain connection to a TLS server is refused", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		c := dial(t, s, Config{CallTimeout: 2 * time.Second})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a plain client got an answer from a TLS server")
		}
		if len(s.Calls()) != 0 {
			t.Fatalf("the server ran %v for a plain client", s.Calls())
		}
	})
	t.Run("TLS client will not talk to a plain server", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{})
		c := dial(t, s, Config{CallTimeout: 2 * time.Second, TLS: TLS{RootCAFile: pki.CAFile}})
		if _, err := c.NewTask(ctx); err == nil {
			t.Fatal("a TLS client talked to a server with no TLS")
		}
		if len(s.Calls()) != 0 {
			t.Fatalf("the server ran %v", s.Calls())
		}
	})
}

func TestDialRefusesWhatCannotWork(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no address":        {},
		"no port":           {Address: "daq-host"},
		"cert without key":  {Address: "h:1", TLS: TLS{CertFile: "a.pem"}},
		"missing root file": {Address: "h:1", TLS: TLS{RootCAFile: "/nonexistent/ca.pem"}},
		"key without cert":  {Address: "h:1", TLS: TLS{KeyFile: "a.key"}},
	} {
		if _, err := Dial(cfg); err == nil {
			t.Errorf("%s: Dial accepted it", name)
		}
	}
}

func TestNothingListeningIsAnError(t *testing.T) {
	c, err := Dial(Config{Address: "127.0.0.1:1", CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NewTask(context.Background()); err == nil {
		t.Fatal("no server, no error")
	}
}
