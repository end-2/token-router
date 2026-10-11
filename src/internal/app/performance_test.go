package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"token-router/src/internal/bucket/backend"
	"token-router/src/internal/config"
	"token-router/src/internal/observability/prometheus"
	"token-router/src/internal/observability/telemetry"
	"token-router/src/internal/proxy"
)

// BenchmarkProxyPipeline measures application work with an in-process upstream
// transport. Network latency and model inference are intentionally excluded.
func BenchmarkProxyPipeline(b *testing.B) {
	for _, name := range []string{"minimal", "defaults", "account", "model", "usage_json", "usage_sse"} {
		b.Run(name, func(b *testing.B) {
			cfg := config.Defaults()
			cfg.RateLimit.RequestsPerSecond, cfg.RateLimit.Burst = 1e12, 1e9
			cfg.Upstream.BaseURLs = []string{"http://upstream.invalid"}
			cfg.Upstream.RequestTimeout = 0
			if name == "minimal" {
				cfg.Telemetry.Logs.Enabled = false
				cfg.Telemetry.Metrics.Enabled = false
			}
			if name == "account" {
				cfg.RateLimit.Account.Enabled = true
			}
			if name == "model" {
				cfg.Upstream.Routes = []config.UpstreamRoute{{Model: "benchmark", BaseURLs: cfg.Upstream.BaseURLs}}
			}
			payload := `{"model":"benchmark","messages":[{"role":"user","content":"hello"}]}`
			response := `{"choices":[{"message":{"content":"` + strings.Repeat("x", 1024) + `"}}],"usage":{"total_tokens":10}}`
			contentType := "application/json"
			if strings.HasPrefix(name, "usage_") {
				cfg.RateLimit.TokensPerSecond, cfg.RateLimit.TokenBurst = 1e12, 1e9
			}
			if name == "usage_sse" {
				contentType = "text/event-stream"
				response = strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", 32) +
					"data: {\"choices\":[],\"usage\":{\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
			}
			telemetry, err := telemetry.New(b.Context(), cfg.Telemetry, io.Discard)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := telemetry.Shutdown(context.Background()); err != nil {
					b.Error(err)
				}
			})
			limiter, err := backend.New(b.Context(), cfg.RateLimit)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := limiter.Close(); err != nil {
					b.Error(err)
				}
			})
			a := &application{cfg: cfg, current: cfg.Mutable(), logger: telemetry.Logger(), limiter: limiter, telemetry: telemetry}
			if cfg.Telemetry.Metrics.Enabled {
				a.metrics, err = prometheus.New()
				if err != nil {
					b.Fatal(err)
				}
			}
			options := []proxy.Option{proxy.WithTransport(func(http.RoundTripper) http.RoundTripper {
				return pipelineTransport{payload: response, contentType: contentType}
			})}
			if cfg.RateLimit.TokensPerSecond > 0 {
				options = append(options, proxy.WithUsageRecorder(a.recordUsage))
			}
			p, err := proxy.New(cfg.Upstream, a.logger, options...)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(p.CloseIdleConnections)
			a.proxy = &proxyGeneration{handler: p}
			a.ready.Store(true)
			handler := a.handler()
			b.ReportAllocs()
			b.SetBytes(int64(len(response)))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/v1/chat/completions", nil)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", accountJWT(`{"sub":"benchmark","iat":1}`))
				writer := &pipelineWriter{header: make(http.Header)}
				for pb.Next() {
					request.Body = io.NopCloser(strings.NewReader(payload))
					request.ContentLength = int64(len(payload))
					clear(writer.header)
					writer.status, writer.bytes = 0, 0
					handler.ServeHTTP(writer, request)
					if writer.status != http.StatusOK || writer.bytes != len(response) {
						b.Errorf("status=%d bytes=%d, want 200 and %d bytes", writer.status, writer.bytes, len(response))
						return
					}
				}
			})
		})
	}
}

type pipelineTransport struct {
	payload, contentType string
}

func (t pipelineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		if err := r.Body.Close(); err != nil {
			return nil, err
		}
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {t.contentType}},
		Body: io.NopCloser(strings.NewReader(t.payload)), ContentLength: int64(len(t.payload)), Request: r,
	}, nil
}

type pipelineWriter struct {
	header        http.Header
	status, bytes int
}

func (w *pipelineWriter) Header() http.Header    { return w.header }
func (w *pipelineWriter) WriteHeader(status int) { w.status = status }
func (w *pipelineWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}
func (*pipelineWriter) Flush()                  {}
func (*pipelineWriter) EnableFullDuplex() error { return nil }

func BenchmarkAccountIdentifier(b *testing.B) {
	for _, tc := range []struct{ name, authorization string }{
		{"basic", accountBasic("benchmark", "password")},
		{"bearer", accountJWT(`{"sub":"benchmark","iss":"issuer","iat":1,"exp":9999999999,"roles":["user"]}`)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			request := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", tc.authorization)
			cfg := config.Account{Enabled: true, JWTClaim: "sub"}
			b.ReportAllocs()
			for b.Loop() {
				if identifier, ok := accountIdentifier(request, cfg); !ok || identifier != "benchmark" {
					b.Fatalf("identifier=%q valid=%t", identifier, ok)
				}
			}
		})
	}
}
