package main

import (
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// A build stamped with a commit says it after the version, the way the image
// is built; one stamped with only a version, or nothing, says no commit. Built
// three times here, the way the Containerfile builds it.
func TestVersionNamesTheCommitAnImageWasBuiltFrom(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, ldflags, want string }{
		{"unstamped", "", "dev"},
		{"version only", "-X main.version=v9.8.7", "v9.8.7"},
		{"version and commit", "-X main.version=v9.8.7 -X main.commit=0123abc", "v9.8.7 (commit 0123abc)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-"))
			if goruntime.GOOS == "windows" {
				bin += ".exe"
			}
			args := []string{"build", "-buildvcs=false"}
			if c.ldflags != "" {
				args = append(args, "-ldflags", c.ldflags)
			}
			if out, err := exec.Command("go", append(args, "-o", bin, ".")...).CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			out, err := exec.Command(bin, "version").Output()
			if err != nil {
				t.Fatalf("version: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Errorf("the %s build says version %q, want %q", c.name, got, c.want)
			}
		})
	}
}
