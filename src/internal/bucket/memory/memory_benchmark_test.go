package memory

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"token-router/src/internal/bucket"
)

func BenchmarkMemoryAllow(b *testing.B) {
	for _, accounts := range []int{1, 256} {
		b.Run("accounts="+strconv.Itoa(accounts), func(b *testing.B) {
			l := New(1e12, 1000, 0, 0)
			contexts := make([]context.Context, accounts)
			for i := range contexts {
				contexts[i] = bucket.WithAccount(context.Background(), strconv.Itoa(i))
				if _, err := l.Allow(contexts[i]); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := l.Allow(contexts[i%accounts]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemoryAllowParallel(b *testing.B) {
	for _, accounts := range []uint64{1, 256} {
		b.Run("accounts="+strconv.FormatUint(accounts, 10), func(b *testing.B) {
			l := New(1e12, 1000, 0, 0)
			contexts := make([]context.Context, accounts)
			for i := range contexts {
				contexts[i] = bucket.WithAccount(context.Background(), strconv.Itoa(i))
				if _, err := l.Allow(contexts[i]); err != nil {
					b.Fatal(err)
				}
			}

			var worker atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := worker.Add(1)
				for pb.Next() {
					if _, err := l.Allow(contexts[i%accounts]); err != nil {
						b.Error(err)
					}
					i++
				}
			})
		})
	}
}
