package observability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

func TestMetricsRecordsErrorsAndBoundsLabels(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.Use(m.Middleware, middleware.Recover())
	e.GET("/failure", func(_ *echo.Context) error { return errors.New("private details") })
	e.GET("/panic", func(_ *echo.Context) error { panic("handler panic") })
	for _, path := range []string{"/failure", "/panic"} {
		response := httptest.NewRecorder()
		e.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if response.Code != 500 {
			t.Fatalf("status=%d", response.Code)
		}
	}
	e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "RANDOM_METHOD", "/unknown/identifier", nil))
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for _, expected := range []string{
		`token_router_http_requests_total{method="GET",route="/failure",status="500"} 1`,
		`token_router_http_requests_total{method="GET",route="/panic",status="500"} 1`,
		`method="OTHER",route="unmatched"`,
		`token_router_http_requests_in_flight 0`,
		`go_goroutines`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("missing %s", expected)
		}
	}
	if strings.Contains(response.Body.String(), "RANDOM_METHOD") || strings.Contains(response.Body.String(), "/unknown/identifier") {
		t.Fatal("unbounded metric label")
	}
	if _, err := New(); err != nil {
		t.Fatalf("independent registry failed: %v", err)
	}
}
