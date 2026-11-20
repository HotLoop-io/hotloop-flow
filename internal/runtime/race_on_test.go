//go:build race

package runtime_test

// The race detector makes every memory access several times dearer, so a CPU
// budget set without it means nothing under it.
const raceDetector = true
