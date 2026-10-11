package bucket

import (
	"math"
	"time"
)

// UsageRetry rounds a refill delay up and saturates at the maximum duration.
func UsageRetry(seconds float64) time.Duration {
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return max(time.Nanosecond, time.Duration(math.Ceil(seconds*float64(time.Second))))
}
