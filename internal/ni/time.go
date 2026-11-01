package ni

import "time"

func durationOf(seconds float64) time.Duration {
	if seconds > 3600*24 {
		seconds = 3600 * 24
	}
	return time.Duration(seconds * float64(time.Second))
}
