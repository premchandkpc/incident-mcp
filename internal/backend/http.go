package backend

import (
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout = 15 * time.Second
	// Cap how much of an upstream error body is echoed back to the caller so a
	// hostile or misconfigured endpoint cannot flood the MCP response.
	maxErrorBodyBytes = 2048
)

// NewHTTPClient returns a client for fixed internal endpoints. It never
// follows redirects, so a bearer token or query string cannot be forwarded to
// an unexpected host, and it applies a hard overall timeout.
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// readErrorBody reads a bounded, trimmed snippet of an error response body.
// Bodies are treated as untrusted: control characters are dropped.
func readErrorBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	if err != nil || len(b) == 0 {
		return ""
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' {
			return ' '
		}
		return r
	}, string(b))
	return strings.TrimSpace(s)
}
