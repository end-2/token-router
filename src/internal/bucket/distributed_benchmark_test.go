package bucket

import (
	"context"
	"testing"
	"time"
)

func BenchmarkDistributedUsageDisabled(b *testing.B) {
	stream := newAdminStream("", "", "", nil)
	b.Cleanup(stream.cancel)
	l := &distributedLimiter{stream: stream, timeout: time.Second}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := l.RecordUsage(ctx, 10); err != nil {
			b.Fatal(err)
		}
	}
}
