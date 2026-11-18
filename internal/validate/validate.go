// Package validate contains input validation helpers shared by configuration
// loading and MCP tool argument handling. Everything a caller controls is
// treated as untrusted: service names become Kubernetes label selectors and
// Prometheus label values, and timestamps become query ranges.
package validate

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	maxDNSLabel = 63
	// A service prefix must leave room for the rest of the DNS label.
	maxServicePrefix = 60
	// Clock skew tolerated when a caller supplies an end timestamp.
	futureSlack = 5 * time.Minute
)

// DNSLabel reports whether s is a valid lowercase DNS label (RFC 1123).
func DNSLabel(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if len(s) > maxDNSLabel {
		return fmt.Errorf("must be at most %d characters, got %d", maxDNSLabel, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 || i == len(s)-1 {
			if !alnum {
				return errors.New("must start and end with a lowercase letter or digit")
			}
			continue
		}
		if !alnum && c != '-' {
			return errors.New("may only contain lowercase letters, digits and '-'")
		}
	}
	return nil
}

// Namespace validates a Kubernetes namespace name.
func Namespace(s string) error {
	if err := DNSLabel(s); err != nil {
		return fmt.Errorf("namespace %q: %w", s, err)
	}
	return nil
}

// ServicePrefix validates the configured ALLOWED_SERVICE_PREFIX. The prefix
// itself may end with '-' (for example "alerts-"), which is not a valid DNS
// label on its own, so it is checked as a label fragment.
func ServicePrefix(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if len(s) > maxServicePrefix {
		return fmt.Errorf("must be at most %d characters, got %d", maxServicePrefix, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 && !alnum {
			return errors.New("must start with a lowercase letter or digit")
		}
		if !alnum && c != '-' {
			return errors.New("may only contain lowercase letters, digits and '-'")
		}
	}
	return nil
}

// Service validates a caller-supplied service name and enforces that it is
// inside the configured namespace of services (prefix allow-list) the MCP
// server is allowed to investigate.
func Service(s, prefix string) error {
	if err := DNSLabel(s); err != nil {
		return fmt.Errorf("service %q: %w", s, err)
	}
	if prefix != "" && !strings.HasPrefix(s, prefix) {
		return fmt.Errorf("service %q is outside the allowed prefix %q", s, prefix)
	}
	return nil
}

// Endpoint validates a fixed backend base URL: it must be an absolute http(s)
// URL without embedded credentials, and must use TLS for anything that is not
// loopback.
func Endpoint(name, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host", name)
	}
	if u.User != nil {
		return fmt.Errorf("%s: credentials in the URL are not allowed", name)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%s: must use https for non-loopback host %q", name, u.Hostname())
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PageLimit resolves a requested page size. A non-positive request selects
// the default; a request above the maximum is clamped and reported so the
// caller can see the truncation rather than silently receiving fewer items
// than asked for.
func PageLimit(requested, defaultLimit, maxLimit int) (limit int, err error) {
	if defaultLimit <= 0 || maxLimit <= 0 || defaultLimit > maxLimit {
		return 0, fmt.Errorf("page limit bounds must be positive and default <= max, got default %d max %d", defaultLimit, maxLimit)
	}
	if requested <= 0 {
		return defaultLimit, nil
	}
	if requested > maxLimit {
		return maxLimit, nil
	}
	return requested, nil
}

// continueToken bounds an opaque Kubernetes continue token. These are
// base64url-encoded, so a strict character class plus a length cap keeps a
// caller from using the field as an injection or amplification vector.
func ContinueToken(s string) error {
	if s == "" {
		return nil
	}
	const maxToken = 4096
	if len(s) > maxToken {
		return fmt.Errorf("continue token must be at most %d characters, got %d", maxToken, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '=' || c == '/'
		if !ok {
			return fmt.Errorf("continue token contains an invalid character at position %d", i)
		}
	}
	return nil
}

// AlertState validates an optional Prometheus alert state filter. Empty means
// "both states"; anything else must be one of the two states Prometheus
// reports, so a typo becomes a clear error instead of silently returning
// everything.
func AlertState(s string) (string, error) {
	switch s {
	case "", "firing", "pending":
		return s, nil
	}
	return "", fmt.Errorf("state must be one of %q, %q or empty (got %q)", "firing", "pending", s)
}

// Window resolves the requested window in minutes against MAX_WINDOW_MINUTES.
// A non-positive request selects the default window; a request above the
// maximum is clamped and reported so callers can see the truncation.
func Window(requested, maxMinutes int) (minutes int, clamped bool, err error) {
	if maxMinutes <= 0 {
		return 0, false, errors.New("max window must be positive")
	}
	const defaultMinutes = 15
	m := requested
	if m <= 0 {
		m = defaultMinutes
		if m > maxMinutes {
			m = maxMinutes
		}
		return m, false, nil
	}
	if m > maxMinutes {
		return maxMinutes, true, nil
	}
	return m, false, nil
}

// TimeRange turns optional RFC3339 start/end strings plus a default window
// into an absolute, bounded range. The returned range never exceeds
// maxMinutes and never ends in the future.
func TimeRange(startStr, endStr string, fallbackMinutes, maxMinutes int, now time.Time) (start, end time.Time, clamped bool, err error) {
	end = now
	if endStr != "" {
		end, err = time.Parse(time.RFC3339, endStr)
		if err != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("invalid end timestamp %q (want RFC3339): %w", endStr, err)
		}
		if end.After(now.Add(futureSlack)) {
			return time.Time{}, time.Time{}, false, fmt.Errorf("end timestamp %s is in the future", end.Format(time.RFC3339))
		}
		if end.After(now) {
			end, clamped = now, true
		}
	}
	start = end.Add(-time.Duration(fallbackMinutes) * time.Minute)
	if startStr != "" {
		start, err = time.Parse(time.RFC3339, startStr)
		if err != nil {
			return time.Time{}, time.Time{}, false, fmt.Errorf("invalid start timestamp %q (want RFC3339): %w", startStr, err)
		}
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("start (%s) must be earlier than end (%s)",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if maxDur := time.Duration(maxMinutes) * time.Minute; end.Sub(start) > maxDur {
		start, clamped = end.Add(-maxDur), true
	}
	return start, end, clamped, nil
}
