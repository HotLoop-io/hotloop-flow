// Package memlimit tells the Go runtime how much memory its container allows.
//
// Without it the collector paces itself on GOGC alone and lets the heap grow to
// twice what is live before it collects. A flow file with thousands of nodes
// holds most of a gigabyte live, so the heap reaches two gigabytes between
// collections and the kernel kills the process at the container's limit, over
// and over, though the live data would have fit. A soft limit below the
// container's makes the collector work harder as the heap nears it instead.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Fraction of the container's limit the Go heap may use. The rest is for what
// the collector does not count: goroutine stacks, the SQLite page cache and
// mapped files, cgo, and the binary itself.
const (
	numerator   = 85
	denominator = 100
)

// cgroup files that hold the limit, v2 first and then v1.
var cgroupFiles = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// Result says what Apply did.
type Result struct {
	// Limit is the soft limit now in force, in bytes. 0 means none was set.
	Limit int64
	// Source is "GOMEMLIMIT", "cgroup", or "" when nothing applies.
	Source string
}

// Apply sets the runtime's soft memory limit from the container's cgroup
// limit. A GOMEMLIMIT in the environment is the operator's own say and wins.
// No limit, or one that is not a number (the cgroup file says "max" when the
// container has none), leaves the runtime as it was.
func Apply() Result {
	return apply(os.Getenv("GOMEMLIMIT"), cgroupFiles, debug.SetMemoryLimit)
}

func apply(env string, files []string, set func(int64) int64) Result {
	if env != "" {
		// The runtime already read it at start.
		return Result{Limit: set(-1), Source: "GOMEMLIMIT"}
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		limit, ok := parse(string(raw))
		if !ok {
			continue
		}
		soft := limit / denominator * numerator
		set(soft)
		return Result{Limit: soft, Source: "cgroup"}
	}
	return Result{}
}

// parse reads one cgroup limit. It reports false for "max", for garbage, and
// for the huge value cgroup v1 shows when no limit is set.
func parse(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 || n >= 1<<60 {
		return 0, false
	}
	return n, true
}
