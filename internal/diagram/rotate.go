package diagram

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Rotation and retention for the log nodes. A log that runs for months is one
// file nobody can open, and a disk that fills stops the machine the log is for,
// so a log can start a new file when this one is big or old and keep only the
// newest few of the ones it has finished.
//
// The finished file is the log's own name with the time it was closed put before
// the extension: bench.csv becomes bench-20261114T120000Z.csv, and the log goes on
// in bench.csv. Retention keeps the newest files of that pattern and deletes the
// rest, and it only ever deletes files that match the pattern, never the live
// log and never a file that merely sits beside it.
type rotation struct {
	maxBytes int64
	maxAge   time.Duration
	keep     int
}

func rotationProps() []node.Prop {
	return []node.Prop{
		prop("rotateMB", "Start a new file after this many megabytes (0 for never)", node.PropNumber, 0),
		prop("rotateHours", "Start a new file after this many hours (0 for never)", node.PropNumber, 0),
		prop("keep", "Keep this many finished files and delete older ones (0 keeps all)", node.PropNumber, 0),
	}
}

func rotationOf(raw map[string]any) (rotation, error) {
	r := rotation{
		maxBytes: int64(floatProp(raw, "rotateMB", 0) * 1024 * 1024),
		maxAge:   time.Duration(floatProp(raw, "rotateHours", 0) * float64(time.Hour)),
		keep:     int(floatProp(raw, "keep", 0)),
	}
	if r.maxBytes < 0 || r.maxAge < 0 || r.keep < 0 {
		return r, fmt.Errorf("rotation settings cannot be negative")
	}
	if r.keep > 0 && r.maxBytes == 0 && r.maxAge == 0 {
		return r, fmt.Errorf("keep only means something with a size or an age to start a new file by")
	}
	return r, nil
}

// due says whether the live file is big or old enough to be finished.
func (r rotation) due(size int64, openedAt, now time.Time) bool {
	if size == 0 {
		return false // never finish an empty file
	}
	if r.maxBytes > 0 && size >= r.maxBytes {
		return true
	}
	return r.maxAge > 0 && now.Sub(openedAt) >= r.maxAge
}

// archiveName is where the finished file goes.
func archiveName(path string, now time.Time) string {
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + "-" + now.UTC().Format("20060102T150405Z") + ext
}

// finish moves the live file aside under its archive name and deletes the oldest
// finished files past the keep count. A name already taken gets a counter, so two
// rotations in one second lose nothing.
func (r rotation) finish(path string, now time.Time) error {
	to := archiveName(path, now)
	for i := 2; ; i++ {
		if _, err := os.Stat(to); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(path)
		to = strings.TrimSuffix(archiveName(path, now), ext) + fmt.Sprintf("-%d", i) + ext
	}
	if err := os.Rename(path, to); err != nil {
		return err
	}
	return r.prune(path)
}

func (r rotation) prune(path string) error {
	if r.keep <= 0 {
		return nil
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var finished []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, base+"-") || !strings.HasSuffix(n, ext) {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(n, base+"-"), ext)
		if len(stamp) < len("20060102T150405Z") {
			continue
		}
		if _, err := time.Parse("20060102T150405Z", stamp[:len("20060102T150405Z")]); err != nil {
			continue
		}
		finished = append(finished, n)
	}
	sort.Strings(finished) // the stamp sorts by time, the counter after it
	for len(finished) > r.keep {
		if err := os.Remove(filepath.Join(dir, finished[0])); err != nil {
			return err
		}
		finished = finished[1:]
	}
	return nil
}
