package app

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// Echo commits its tracked status on every WriteHeader, including informational responses.
type proxyResponseWriter struct {
	http.ResponseWriter
}

func (writer proxyResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		if response, err := echo.UnwrapResponse(writer.ResponseWriter); err == nil {
			response.Unwrap().WriteHeader(status)
			return
		}
	}

	writer.ResponseWriter.WriteHeader(status)
}

func (writer proxyResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}
