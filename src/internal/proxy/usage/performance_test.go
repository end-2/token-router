package usage

import (
	"context"
	"testing"
)

func BenchmarkUsageEvents(b *testing.B) {
	for _, test := range []struct{ name, payload string }{
		{name: "Content", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"},
		{name: "NullUsage", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":null}\n\n"},
		{name: "Usage", payload: "data: {\"usage\":{\"total_tokens\":12}}\n\n"},
	} {
		b.Run(test.name, func(b *testing.B) {
			body := &usageBody{ctx: context.Background(), record: func(context.Context, int64) {}}
			events := usageEvents{observe: body.observe}
			payload := []byte(test.payload)

			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				events.feed(payload)
			}
		})
	}
}
