// Package nitest is a stand-in for NI's gRPC Device Server, for tests. It speaks
// gRPC over HTTP/2, plain or with TLS, and answers the DAQmx calls Flow's client
// makes with a sine wave on every channel. It is NOT NI's server: it has never
// been compared with one, and a test that passes against it proves only that the
// client does what the stand-in was written to accept. Nothing outside tests
// imports this package.
package nitest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/HotLoop-io/hotloop-flow/internal/ni/proto"
	"google.golang.org/protobuf/proto"
)

// Error codes the stand-in answers with. -200220 is NI's own code for an invalid
// device ID (NIDAQmx.h, DAQmxErrorInvalidDeviceID); the text is the stand-in's.
// CodeStandInFault is not an NI code: it is a made up negative number for a
// failure the stand-in invents.
const (
	CodeInvalidDevice = -200220
	CodeStandInFault  = -209999
)

// StandInServer is the stand-in. Make one with NewStandIn.
type StandInServer struct {
	// Addr is host:port to dial.
	Addr string

	mu    sync.Mutex
	tasks map[string]*standInTask
	calls []string
	srv   *http.Server
	done  chan struct{}

	hang     atomic.Bool
	failNext atomic.Value // string: the method to fail once with CodeStandInFault
	sawTLS   atomic.Int32
	sawPeer  atomic.Int32
}

type standInChan struct {
	physical string
	count    int
}

type standInTask struct {
	chans   []standInChan
	rate    float64
	mode    pb.AcquisitionType
	samples uint64
	started bool
	drawn   int64
	trigger string
}

// Options for NewStandIn.
type Options struct {
	// ServerCert and ServerKey turn TLS on (PEM files).
	ServerCert, ServerKey string
	// ClientCA, when set with TLS, makes the stand-in demand a client
	// certificate signed by that authority: mutual TLS.
	ClientCA string
}

// NewStandIn starts a stand-in on 127.0.0.1 and stops it when the test ends.
func NewStandIn(t testing.TB, o Options) *StandInServer {
	t.Helper()
	s := &StandInServer{tasks: map[string]*standInTask{}, done: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Addr = ln.Addr().String()
	s.srv = &http.Server{Handler: s, Protocols: new(http.Protocols), ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	if o.ServerCert != "" {
		cert, err := tls.LoadX509KeyPair(o.ServerCert, o.ServerKey)
		if err != nil {
			t.Fatal(err)
		}
		s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}
		if o.ClientCA != "" {
			pool := x509.NewCertPool()
			pem, err := readFile(o.ClientCA)
			if err != nil {
				t.Fatal(err)
			}
			pool.AppendCertsFromPEM(pem)
			s.srv.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
			s.srv.TLSConfig.ClientCAs = pool
		}
		s.srv.Protocols.SetHTTP2(true)
		go s.srv.ServeTLS(ln, "", "")
	} else {
		s.srv.Protocols.SetUnencryptedHTTP2(true)
		go s.srv.Serve(ln)
	}
	t.Cleanup(func() {
		close(s.done)
		s.srv.Close()
	})
	return s
}

// Hang makes the stand-in accept calls and never answer them.
func (s *StandInServer) Hang(on bool) { s.hang.Store(on) }

// FailNext makes the next call to method answer with a driver style error.
func (s *StandInServer) FailNext(method string) { s.failNext.Store(method) }

// Calls lists the methods called so far, in order.
func (s *StandInServer) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Tasks is how many tasks the stand-in holds right now.
func (s *StandInServer) Tasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

// PeerCerts is how many calls came with a verified client certificate.
func (s *StandInServer) PeerCerts() int { return int(s.sawPeer.Load()) }

// Value is the stand-in's sample: channel c, sample i of a task sampling at
// rate. Channel 0 is a 5 Hz sine of amplitude 1, channel 1 a 10 Hz sine of
// amplitude 2, and so on. Tests use it to check order and spacing.
func Value(c int, i int64, rate float64) float64 {
	return float64(c+1) * math.Sin(2*math.Pi*5*float64(c+1)*float64(i)/rate)
}

func (s *StandInServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/nidaqmx_grpc.NiDAQmx/")
	if r.TLS != nil {
		s.sawTLS.Add(1)
		if len(r.TLS.VerifiedChains) > 0 {
			s.sawPeer.Add(1)
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<24))
	if err != nil || len(body) < 5 || r.Header.Get("Content-Type") != "application/grpc" {
		trailersOnly(w, 3, 0, "stand-in: not a gRPC request")
		return
	}
	if n := binary.BigEndian.Uint32(body[1:5]); int(n) != len(body)-5 {
		trailersOnly(w, 3, 0, "stand-in: bad frame length")
		return
	}
	payload := body[5:]
	s.mu.Lock()
	s.calls = append(s.calls, method)
	s.mu.Unlock()
	if s.hang.Load() {
		select {
		case <-r.Context().Done():
		case <-s.done:
		}
		return
	}
	if f, _ := s.failNext.Load().(string); f != "" && f == method {
		s.failNext.Store("")
		trailersOnly(w, 2, CodeStandInFault, "stand-in: the task could not be started")
		return
	}
	var out proto.Message
	var code int32
	var msg string
	switch method {
	case "CreateTask":
		out, code, msg = s.createTask(payload)
	case "CreateAIVoltageChan":
		out, code, msg = s.addChan(payload, new(pb.CreateAIVoltageChanRequest), new(pb.CreateAIVoltageChanResponse))
	case "CreateAICurrentChan":
		out, code, msg = s.addChan(payload, new(pb.CreateAICurrentChanRequest), new(pb.CreateAICurrentChanResponse))
	case "CreateAIThrmcplChan":
		out, code, msg = s.addChan(payload, new(pb.CreateAIThrmcplChanRequest), new(pb.CreateAIThrmcplChanResponse))
	case "CfgSampClkTiming":
		out, code, msg = s.sampClk(payload)
	case "CfgDigEdgeStartTrig":
		out, code, msg = s.startTrig(payload)
	case "StartTask":
		out, code, msg = s.control(payload, new(pb.StartTaskRequest), new(pb.StartTaskResponse), "start")
	case "StopTask":
		out, code, msg = s.control(payload, new(pb.StopTaskRequest), new(pb.StopTaskResponse), "stop")
	case "ClearTask":
		out, code, msg = s.control(payload, new(pb.ClearTaskRequest), new(pb.ClearTaskResponse), "clear")
	case "ReadAnalogF64":
		out, code, msg = s.read(payload)
	default:
		trailersOnly(w, 12, 0, "stand-in: no such method "+method)
		return
	}
	if out == nil {
		g := 2
		if code == 0 {
			g = 3
		}
		trailersOnly(w, g, code, msg)
		return
	}
	b, err := proto.Marshal(out)
	if err != nil {
		trailersOnly(w, 13, 0, err.Error())
		return
	}
	frame := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(b)))
	copy(frame[5:], b)
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	w.Write(frame)
	w.Header().Set("Grpc-Status", "0")
	w.Header().Set("Grpc-Message", "")
}

// trailersOnly answers the way a gRPC server answers a call that failed before
// it had a message to send: the status rides in the headers.
func trailersOnly(w http.ResponseWriter, grpc int, ni int32, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Status", strconv.Itoa(grpc))
	h.Set("Grpc-Message", msg)
	if ni != 0 {
		h.Set("Ni-Error", strconv.Itoa(int(ni)))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *StandInServer) task(name string) (*standInTask, bool) {
	t, ok := s.tasks[name]
	return t, ok
}

func (s *StandInServer) createTask(p []byte) (proto.Message, int32, string) {
	var req pb.CreateTaskRequest
	if proto.Unmarshal(p, &req) != nil || req.GetSessionName() == "" {
		return nil, 0, "stand-in: a task needs a session name"
	}
	if req.GetInitializationBehavior() != pb.SessionInitializationBehavior_SESSION_INITIALIZATION_BEHAVIOR_INITIALIZE_NEW {
		return nil, 0, "stand-in: only a new session is made"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.tasks[req.GetSessionName()]; dup {
		return nil, 0, "stand-in: that session already exists"
	}
	s.tasks[req.GetSessionName()] = &standInTask{}
	return &pb.CreateTaskResponse{Task: &pb.Session{Name: req.GetSessionName()}, NewSessionInitialized: true}, 0, ""
}

type chanReq interface {
	proto.Message
	GetTask() *pb.Session
	GetPhysicalChannel() string
}

func (s *StandInServer) addChan(p []byte, req chanReq, resp proto.Message) (proto.Message, int32, string) {
	if proto.Unmarshal(p, req) != nil {
		return nil, 0, "stand-in: bad request"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.task(req.GetTask().GetName())
	if !ok {
		return nil, 0, "stand-in: no such task"
	}
	phys := req.GetPhysicalChannel()
	if !strings.HasPrefix(phys, "cDAQ1Mod1/") {
		return nil, CodeInvalidDevice, "stand-in: the device in " + strconv.Quote(phys) + " is not in this chassis, which holds cDAQ1Mod1"
	}
	t.chans = append(t.chans, standInChan{physical: phys, count: rangeCount(phys)})
	return resp, 0, ""
}

func rangeCount(phys string) int {
	n := 0
	for _, part := range strings.Split(phys, ",") {
		i := strings.LastIndex(part, ":")
		if i < 0 {
			n++
			continue
		}
		lo, hi := part[:i], part[i+1:]
		j := len(lo)
		for j > 0 && lo[j-1] >= '0' && lo[j-1] <= '9' {
			j--
		}
		a, _ := strconv.Atoi(lo[j:])
		b, _ := strconv.Atoi(hi)
		n += b - a + 1
	}
	return n
}

func (s *StandInServer) sampClk(p []byte) (proto.Message, int32, string) {
	var req pb.CfgSampClkTimingRequest
	if proto.Unmarshal(p, &req) != nil {
		return nil, 0, "stand-in: bad request"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.task(req.GetTask().GetName())
	if !ok {
		return nil, 0, "stand-in: no such task"
	}
	if req.GetRate() <= 0 {
		return nil, 0, "stand-in: the rate must be above zero"
	}
	t.rate, t.mode, t.samples = req.GetRate(), req.GetSampleMode(), req.GetSampsPerChan()
	return &pb.CfgSampClkTimingResponse{}, 0, ""
}

func (s *StandInServer) startTrig(p []byte) (proto.Message, int32, string) {
	var req pb.CfgDigEdgeStartTrigRequest
	if proto.Unmarshal(p, &req) != nil {
		return nil, 0, "stand-in: bad request"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.task(req.GetTask().GetName())
	if !ok {
		return nil, 0, "stand-in: no such task"
	}
	t.trigger = fmt.Sprintf("%s/%v", req.GetTriggerSource(), req.GetTriggerEdge())
	return &pb.CfgDigEdgeStartTrigResponse{}, 0, ""
}

type taskReq interface {
	proto.Message
	GetTask() *pb.Session
}

func (s *StandInServer) control(p []byte, req taskReq, resp proto.Message, what string) (proto.Message, int32, string) {
	if proto.Unmarshal(p, req) != nil {
		return nil, 0, "stand-in: bad request"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.GetTask().GetName()
	t, ok := s.task(name)
	if !ok {
		return nil, 0, "stand-in: no such task"
	}
	switch what {
	case "start":
		if len(t.chans) == 0 || t.rate == 0 {
			return nil, CodeStandInFault, "stand-in: the task has no channels or no sample clock to start"
		}
		t.started = true
	case "stop":
		t.started = false
	case "clear":
		delete(s.tasks, name)
	}
	return resp, 0, ""
}

func (s *StandInServer) read(p []byte) (proto.Message, int32, string) {
	var req pb.ReadAnalogF64Request
	if proto.Unmarshal(p, &req) != nil {
		return nil, 0, "stand-in: bad request"
	}
	if _, set := req.GetFillModeEnum().(*pb.ReadAnalogF64Request_FillMode); !set {
		// NI's server refuses a read that does not say how to fill the array.
		return nil, 0, "The value for fill_mode was not specified or out of range"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.task(req.GetTask().GetName())
	if !ok {
		return nil, 0, "stand-in: no such task"
	}
	nch := 0
	for _, c := range t.chans {
		nch += c.count
	}
	n := int(req.GetNumSampsPerChan())
	if n <= 0 || nch == 0 {
		return nil, 0, "stand-in: nothing to read"
	}
	if int(req.GetArraySizeInSamps()) < n*nch {
		return nil, 0, "stand-in: the array is smaller than the samples asked for"
	}
	if req.GetFillMode() != pb.GroupBy_GROUP_BY_GROUP_BY_CHANNEL {
		return nil, 0, "stand-in: only group by channel is made"
	}
	out := make([]float64, n*nch)
	for c := 0; c < nch; c++ {
		for i := 0; i < n; i++ {
			out[c*n+i] = Value(c, t.drawn+int64(i), t.rate)
		}
	}
	t.drawn += int64(n)
	return &pb.ReadAnalogF64Response{ReadArray: out, SampsPerChanRead: int32(n)}, 0, ""
}

// WaitCalls is for tests that need the stand-in to have seen n calls.
func (s *StandInServer) WaitCalls(ctx context.Context, n int) bool {
	for {
		s.mu.Lock()
		got := len(s.calls)
		s.mu.Unlock()
		if got >= n {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}
