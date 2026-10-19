// Package otlp sends a trace's hops to an OpenTelemetry collector as spans.
//
// It speaks OTLP over HTTP with the JSON encoding, which the OpenTelemetry
// specification defines (https://opentelemetry.io/docs/specs/otlp/) and every
// collector's OTLP receiver accepts on port 4318. That is the whole protocol,
// and writing it out by hand costs about two hundred lines. The alternative is
// the OpenTelemetry SDK and its protobuf and gRPC dependencies, which would
// add more to a binary that is held to a size budget than the rest of this
// feature put together, to do the same single POST.
//
// What is sent is the shape of the journey and its timing: which node, which
// kind of node, how long, whether it failed, which message id. The messages
// themselves never leave: a payload is a plant's data, and a collector is
// somebody else's server.
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Config is where spans go and how they are batched.
type Config struct {
	// Endpoint is the collector's OTLP/HTTP address, like
	// http://collector:4318. /v1/traces is added to it.
	Endpoint string
	// Headers go on every request: an API key a hosted collector wants.
	Headers map[string]string
	// ServiceName is the service.name resource attribute.
	ServiceName string
	// ServiceVersion is the service.version resource attribute.
	ServiceVersion string
	// Interval is the longest a finished span waits before it is sent.
	Interval time.Duration
	// Timeout bounds one request.
	Timeout time.Duration
	// BatchSize is the most spans in one request.
	BatchSize int
	// QueueSize is how many finished spans wait to be sent. A full queue drops
	// the newest and counts it: a collector that is down must not make the
	// flow slower or bigger.
	QueueSize int
}

func (c *Config) defaults() {
	if c.ServiceName == "" {
		c.ServiceName = "hotloop-flow"
	}
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 512
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 8192
	}
}

// Exporter batches spans and sends them.
type Exporter struct {
	cfg    Config
	url    string
	client *http.Client
	log    *slog.Logger

	queue chan engine.Span
	done  chan struct{}
	once  sync.Once

	exported atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64
}

// Stats are what the exporter has done.
type Stats struct {
	Exported int64
	Dropped  int64
	Failed   int64
}

// New checks the configuration and returns an exporter. Call Run to start it.
func New(cfg Config, log *slog.Logger) (*Exporter, error) {
	cfg.defaults()
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("trace.otlp.endpoint %q is not an http or https address like http://collector:4318", cfg.Endpoint)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/traces"
	if log == nil {
		log = slog.Default()
	}
	return &Exporter{
		cfg:    cfg,
		url:    u.String(),
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log,
		queue:  make(chan engine.Span, cfg.QueueSize),
		done:   make(chan struct{}),
	}, nil
}

// Add queues a finished hop. It never blocks.
func (e *Exporter) Add(s engine.Span) {
	select {
	case e.queue <- s:
	default:
		e.dropped.Add(1)
	}
}

// Stats reports the counters.
func (e *Exporter) Stats() Stats {
	return Stats{Exported: e.exported.Load(), Dropped: e.dropped.Load(), Failed: e.failed.Load()}
}

// Run sends batches until ctx ends, then sends what is left and returns.
func (e *Exporter) Run(ctx context.Context) {
	defer close(e.done)
	tick := time.NewTicker(e.cfg.Interval)
	defer tick.Stop()
	batch := make([]engine.Span, 0, e.cfg.BatchSize)
	flush := func(ctx context.Context) {
		if len(batch) > 0 {
			e.send(ctx, batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case s := <-e.queue:
			batch = append(batch, s)
			if len(batch) >= e.cfg.BatchSize {
				flush(ctx)
			}
		case <-tick.C:
			flush(ctx)
		case <-ctx.Done():
			// Whatever finished before the stop still goes, on a fresh
			// deadline, because the context that ended is the reason this
			// was called.
			end, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout)
			defer cancel()
			for {
				select {
				case s := <-e.queue:
					batch = append(batch, s)
					if len(batch) >= e.cfg.BatchSize {
						flush(end)
					}
				default:
					flush(end)
					return
				}
			}
		}
	}
}

// Wait blocks until Run has returned.
func (e *Exporter) Wait() { <-e.done }

// Flush sends everything queued now, for a test or a shutdown that wants the
// answer before it goes on.
func (e *Exporter) Flush(ctx context.Context) error {
	var batch []engine.Span
	for {
		select {
		case s := <-e.queue:
			batch = append(batch, s)
			continue
		default:
		}
		break
	}
	if len(batch) == 0 {
		return nil
	}
	for len(batch) > 0 {
		n := min(len(batch), e.cfg.BatchSize)
		if err := e.post(ctx, batch[:n]); err != nil {
			e.failed.Add(int64(n))
			return err
		}
		e.exported.Add(int64(n))
		batch = batch[n:]
	}
	return nil
}

func (e *Exporter) send(ctx context.Context, batch []engine.Span) {
	err := e.post(ctx, batch)
	if err != nil {
		// One more try, because the usual reason is a collector restarting.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
		}
		err = e.post(ctx, batch)
	}
	if err != nil {
		e.failed.Add(int64(len(batch)))
		e.log.Warn("the OpenTelemetry collector didn't take a batch of spans", "spans", len(batch), "error", err.Error())
		return
	}
	e.exported.Add(int64(len(batch)))
}

func (e *Exporter) post(ctx context.Context, batch []engine.Span) error {
	body, err := json.Marshal(e.request(batch))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(reply)))
	}
	var ok struct {
		PartialSuccess struct {
			Rejected json.Number `json:"rejectedSpans"`
			Message  string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(reply, &ok) == nil {
		if n, _ := ok.PartialSuccess.Rejected.Int64(); n > 0 {
			// Not retried: the spec says a client must not retry a partial
			// success, and the same spans would be refused again.
			return errors.New("the collector rejected " + strconv.FormatInt(n, 10) + " spans: " + ok.PartialSuccess.Message)
		}
	}
	return nil
}

// The JSON shapes below are the OTLP ones with only the fields we send.

type exportRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}
type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}
type resource struct {
	Attributes []keyValue `json:"attributes"`
}
type scopeSpans struct {
	Scope scope  `json:"scope"`
	Spans []span `json:"spans"`
}
type scope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}
type span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []keyValue `json:"attributes,omitempty"`
	Links             []link     `json:"links,omitempty"`
	Status            *status    `json:"status,omitempty"`
}
type link struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}
type status struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}
type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}
type anyValue struct {
	String *string `json:"stringValue,omitempty"`
	Int    *string `json:"intValue,omitempty"`
}

func str(k, v string) keyValue { return keyValue{Key: k, Value: anyValue{String: &v}} }
func num(k string, v int64) keyValue {
	s := strconv.FormatInt(v, 10)
	return keyValue{Key: k, Value: anyValue{Int: &s}}
}

// spanKindInternal and statusError are the enum values from the OTLP trace
// proto; the JSON encoding sends enums as integers.
const (
	spanKindInternal = 1
	statusError      = 2
)

func (e *Exporter) request(batch []engine.Span) exportRequest {
	spans := make([]span, 0, len(batch))
	for _, s := range batch {
		h := s.Hop
		sp := span{
			TraceID:           s.TraceID,
			SpanID:            s.Trace.SpanID(h.ID),
			Name:              h.Type + " " + h.Node,
			Kind:              spanKindInternal,
			StartTimeUnixNano: strconv.FormatInt(h.At.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(h.At.UnixNano()+h.Nanos, 10),
			Attributes: []keyValue{
				str("flow.node.id", h.Node),
				str("flow.node.type", h.Type),
				str("flow.message.id", h.MsgID),
				num("flow.hop.sent", int64(h.Sent)),
			},
		}
		if h.Entry != "" {
			sp.Attributes = append(sp.Attributes, str("flow.hop.entry", h.Entry))
		}
		if len(h.Parents) > 0 {
			sp.Attributes = append(sp.Attributes, num("flow.hop.port", int64(h.Port)))
			sp.ParentSpanID = s.Trace.SpanID(h.Parents[0])
			for _, p := range h.Parents[1:] {
				sp.Links = append(sp.Links, link{TraceID: s.TraceID, SpanID: s.Trace.SpanID(p)})
			}
		}
		if h.Via != "" {
			sp.Attributes = append(sp.Attributes, str("flow.hop.via", h.Via))
		}
		if h.Error != "" {
			sp.Status = &status{Code: statusError, Message: h.Error}
		}
		spans = append(spans, sp)
	}
	res := resource{Attributes: []keyValue{str("service.name", e.cfg.ServiceName)}}
	if e.cfg.ServiceVersion != "" {
		res.Attributes = append(res.Attributes, str("service.version", e.cfg.ServiceVersion))
	}
	return exportRequest{ResourceSpans: []resourceSpans{{
		Resource:   res,
		ScopeSpans: []scopeSpans{{Scope: scope{Name: "hotloop-flow/trace", Version: e.cfg.ServiceVersion}, Spans: spans}},
	}}}
}
