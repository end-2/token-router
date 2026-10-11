package routing

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"token-router/src/internal/config"
)

func BenchmarkRouting(b *testing.B) {
	for _, test := range []struct {
		name, payload string
		routes        []config.UpstreamRoute
	}{
		{name: "Default"},
		{name: "Host", routes: []config.UpstreamRoute{{Host: "api.example.com", BaseURLs: []string{"http://upstream.example"}}}},
		{name: "Model64KiB", payload: `{"model":"chat","input":"` + strings.Repeat("x", 64<<10) + `"}`, routes: []config.UpstreamRoute{{Model: "chat", BaseURLs: []string{"http://upstream.example"}}}},
	} {
		b.Run(test.name, func(b *testing.B) {
			cfg := config.Defaults().Upstream
			cfg.BaseURLs = []string{"http://upstream.example"}
			cfg.Routes = test.routes
			routing, err := New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "http://api.example.com/v1/responses", nil)
			request.Header.Set("Content-Type", "application/json")
			request.ContentLength = int64(len(test.payload))
			reader := strings.NewReader(test.payload)
			body := io.NopCloser(reader)
			writer := httptest.NewRecorder()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				reader.Reset(test.payload)
				request.Body = body
				if _, routeErr := routing.SelectTarget(writer, request); routeErr != nil {
					b.Fatal(routeErr)
				}
			}
		})
	}
}
