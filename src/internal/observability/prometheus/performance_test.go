package prometheus

import "testing"

func BenchmarkPrometheusRecordLimit(b *testing.B) {
	metrics, err := New()
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for b.Loop() {
		metrics.RecordLimit(true, nil)
	}
}
