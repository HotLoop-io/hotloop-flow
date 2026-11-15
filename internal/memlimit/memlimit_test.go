package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyUsesEightyFivePercentOfTheCgroupLimit(t *testing.T) {
	var got int64
	r := apply("", []string{write(t, "2147483648\n")}, func(n int64) int64 { got = n; return 0 })
	want := int64(2147483648) / 100 * 85
	if got != want || r.Limit != want || r.Source != "cgroup" {
		t.Fatalf("limit set %d, result %+v, want %d from cgroup", got, r, want)
	}
}

func TestApplyLeavesTheRuntimeAloneWithoutALimit(t *testing.T) {
	for name, body := range map[string]string{
		"v2 unlimited": "max\n",
		"v1 unlimited": "9223372036854771712\n",
		"garbage":      "lots",
		"empty":        "",
		"zero":         "0",
	} {
		called := false
		r := apply("", []string{write(t, body)}, func(int64) int64 { called = true; return 0 })
		if called || r != (Result{}) {
			t.Errorf("%s: set a limit anyway: called=%v result=%+v", name, called, r)
		}
	}
	if r := apply("", []string{"/nonexistent/memory.max"}, func(int64) int64 { t.Fatal("set"); return 0 }); r != (Result{}) {
		t.Errorf("missing file: %+v", r)
	}
}

func TestApplyFallsThroughToTheNextFile(t *testing.T) {
	var got int64
	apply("", []string{"/nonexistent", write(t, "1000000000")}, func(n int64) int64 { got = n; return 0 })
	if got != 850000000 {
		t.Fatalf("got %d", got)
	}
}

func TestGOMEMLIMITWins(t *testing.T) {
	var calls []int64
	r := apply("1GiB", []string{write(t, "2147483648")}, func(n int64) int64 { calls = append(calls, n); return 1 << 30 })
	if r.Source != "GOMEMLIMIT" || r.Limit != 1<<30 {
		t.Fatalf("result %+v", r)
	}
	if len(calls) != 1 || calls[0] != -1 {
		t.Fatalf("it should only read the limit, calls %v", calls)
	}
}
