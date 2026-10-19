package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// helmConfig renders the chart with the given arguments and returns the
// config.yaml its ConfigMap carries, which is the file the binary reads in a
// cluster.
func helmConfig(t *testing.T, args ...string) string {
	t.Helper()
	return configFromRender(t, helmRender(t, args...))
}

// helmRender is what `helm template` prints for the chart with those arguments.
func helmRender(t *testing.T, args ...string) []byte {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("HOTLOOP_FLOW_TEST_HELM") != "" {
			t.Fatalf("HOTLOOP_FLOW_TEST_HELM is set and helm is not on the PATH: %v", err)
		}
		t.Skip("helm is not installed; the chart job in CI runs this with HOTLOOP_FLOW_TEST_HELM set")
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "charts", "hotloop-flow"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helm, append([]string{"template", "t", chart, "--set", "auth.enabled=false"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	return out
}

// configFromRender picks the config.yaml out of a render.
func configFromRender(t *testing.T, out []byte) string {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("reading the render: %v", err)
		}
		if doc.Kind == "ConfigMap" && doc.Data["config.yaml"] != "" {
			return doc.Data["config.yaml"]
		}
	}
	t.Fatal("the chart rendered no config.yaml")
	return ""
}

func loadRendered(t *testing.T, config string) Config {
	t.Helper()
	clearEnv(t)
	t.Setenv("HOTLOOP_FLOW_INSECURE", "true")
	cfg, err := Load(writeConfig(t, config))
	if err != nil {
		t.Fatalf("the binary refuses the config the chart renders: %v\n%s", err, config)
	}
	return cfg
}

// The chart's defaults are the binary's defaults. A value in values.yaml that
// drifts from config.go is a setting that says one thing and does another, and
// nothing before a cluster would notice.
func TestChartDefaultsAreTheConfigDefaults(t *testing.T) {
	got := loadRendered(t, helmConfig(t))
	want := Default()
	if !reflect.DeepEqual(got.Trace, want.Trace) {
		t.Errorf("the chart renders trace %+v, the binary defaults to %+v", got.Trace, want.Trace)
	}
	if !reflect.DeepEqual(got.History, want.History) {
		t.Errorf("the chart renders history %+v, the binary defaults to %+v", got.History, want.History)
	}
	if !reflect.DeepEqual(got.Audit, want.Audit) {
		t.Errorf("the chart renders audit %+v, the binary defaults to %+v", got.Audit, want.Audit)
	}
	if !reflect.DeepEqual(got.Tests, want.Tests) {
		t.Errorf("the chart renders tests %+v, the binary defaults to %+v", got.Tests, want.Tests)
	}
	if !reflect.DeepEqual(got.Store, want.Store) {
		t.Errorf("the chart renders store %+v, the binary defaults to %+v", got.Store, want.Store)
	}
}

// Every trace.* and store.* value reaches the field of the same name.
func TestChartTraceAndStoreValuesReachTheConfig(t *testing.T) {
	got := loadRendered(t, helmConfig(t,
		"--set", "trace.enabled=false",
		"--set", "trace.traces=11",
		"--set", "trace.maxHops=22",
		"--set", "trace.maxMessageBytes=4096",
		"--set", "trace.maxBytes=33554432",
		"--set", "trace.perSecond=7",
		"--set", "trace.otlp.endpoint=http://collector:4318",
		"--set", "trace.otlp.serviceName=line3-flow",
		"--set", "trace.otlp.interval=5s",
		"--set", "store.path=/data/other.db",
		"--set", "store.synchronous=full",
		"--set", "store.context.persist=false",
		"--set", "store.context.flushInterval=0s",
		"--set", "store.durable.maxBytes=134217728",
		"--set", "store.durable.ackFlush=0s",
	))
	tr := got.Trace
	if tr.Enabled || tr.Traces != 11 || tr.MaxHops != 22 || tr.MaxMessageBytes != 4096 || tr.MaxBytes != 33554432 || tr.PerSecond != 7 {
		t.Errorf("trace = %+v", tr)
	}
	if tr.OTLP.Endpoint != "http://collector:4318" || tr.OTLP.ServiceName != "line3-flow" || tr.OTLP.Interval != 5*time.Second {
		t.Errorf("trace.otlp = %+v", tr.OTLP)
	}
	st := got.Store
	if st.Path != "/data/other.db" || st.Synchronous != "full" || st.Context.Persist || st.Context.FlushInterval != 0 {
		t.Errorf("store = %+v", st)
	}
	if st.Durable.MaxBytes != 134217728 || st.Durable.AckFlush != 0 {
		t.Errorf("store.durable = %+v", st.Durable)
	}
}

// Every history.*, audit.* and tests.* value reaches the field of the same
// name, and the git token reaches the pod as the variable the binary reads,
// from a Secret, never the ConfigMap.
func TestChartHistoryAuditAndTestsValuesReachTheConfig(t *testing.T) {
	args := []string{
		"--set", "history.retain=7",
		"--set", "history.git.url=https://git.plant.example/line3/flows.git",
		"--set", "history.git.branch=deploys",
		"--set", "history.git.path=line3/flows.json",
		"--set", "history.git.emailDomain=plant.example",
		"--set", "history.git.username=flowbot",
		"--set", "history.git.passwordSecret.name=flow-git",
		"--set", "audit.maxBytes=1048576",
		"--set", "audit.keep=9",
		"--set", "tests.gate=true",
		"--set", "tests.timeout=45s",
	}
	render := helmRender(t, args...)
	got := loadRendered(t, configFromRender(t, render))
	if got.History.Retain != 7 {
		t.Errorf("history.retain = %d", got.History.Retain)
	}
	g := got.History.Git
	if g.URL != "https://git.plant.example/line3/flows.git" || g.Branch != "deploys" || g.Path != "line3/flows.json" || g.EmailDomain != "plant.example" || g.Username != "flowbot" {
		t.Errorf("history.git = %+v", g)
	}
	if g.Password != "" {
		t.Errorf("the token is in the ConfigMap: %q", g.Password)
	}
	if got.Audit.MaxBytes != 1048576 || got.Audit.Keep != 9 {
		t.Errorf("audit = %+v", got.Audit)
	}
	if !got.Tests.Gate || got.Tests.Timeout != 45*time.Second {
		t.Errorf("tests = %+v", got.Tests)
	}
	if !bytes.Contains(render, []byte("name: HOTLOOP_FLOW_GIT_PASSWORD")) || !bytes.Contains(render, []byte("name: flow-git")) {
		t.Errorf("the deployment does not take HOTLOOP_FLOW_GIT_PASSWORD from the flow-git Secret")
	}
	if bytes.Contains(helmRender(t), []byte("HOTLOOP_FLOW_GIT_PASSWORD")) {
		t.Errorf("a default render sets HOTLOOP_FLOW_GIT_PASSWORD")
	}
}
