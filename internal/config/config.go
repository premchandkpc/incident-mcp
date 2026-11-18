// Package config loads and validates the server configuration from the
// environment. Validation is strict: the process refuses to start with an
// endpoint that would send a Kubernetes bearer token over plaintext HTTP to a
// non-loopback host, or with a scope that is not a valid Kubernetes namespace.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"incident-mcp/internal/validate"
)

// DefaultMaxWindowMinutes is used when MAX_WINDOW_MINUTES is unset.
const DefaultMaxWindowMinutes = 60

// Absolute upper bound for MAX_WINDOW_MINUTES (one week).
const maxWindowMinutesCap = 10080

// Config is the fully validated runtime configuration.
type Config struct {
	PrometheusURL        string
	KubernetesURL        string
	KubernetesToken      string
	AllowedNamespace     string
	AllowedServicePrefix string
	MaxWindowMinutes     int
}

// Load reads configuration from the process environment.
func Load() (Config, error) { return FromEnv(os.Getenv) }

// FromEnv reads configuration using the supplied getter, which makes the
// loader straightforward to test.
func FromEnv(get func(string) string) (Config, error) {
	var c Config
	c.PrometheusURL = strings.TrimSpace(get("PROMETHEUS_URL"))
	c.KubernetesURL = strings.TrimSpace(get("KUBERNETES_URL"))
	c.KubernetesToken = strings.TrimSpace(get("KUBERNETES_TOKEN"))
	c.AllowedNamespace = strings.TrimSpace(get("ALLOWED_NAMESPACE"))
	c.AllowedServicePrefix = strings.TrimSpace(get("ALLOWED_SERVICE_PREFIX"))

	var problems []string
	if err := validate.Endpoint("PROMETHEUS_URL", c.PrometheusURL); err != nil {
		problems = append(problems, err.Error())
	} else {
		c.PrometheusURL = strings.TrimRight(c.PrometheusURL, "/")
	}
	if err := validate.Endpoint("KUBERNETES_URL", c.KubernetesURL); err != nil {
		problems = append(problems, err.Error())
	} else {
		c.KubernetesURL = strings.TrimRight(c.KubernetesURL, "/")
	}
	if err := validate.Namespace(c.AllowedNamespace); err != nil {
		problems = append(problems, fmt.Sprintf("ALLOWED_NAMESPACE: %v", err))
	}
	if err := validate.ServicePrefix(c.AllowedServicePrefix); err != nil {
		problems = append(problems, fmt.Sprintf("ALLOWED_SERVICE_PREFIX: %v", err))
	}

	rawMax := strings.TrimSpace(get("MAX_WINDOW_MINUTES"))
	max := DefaultMaxWindowMinutes
	if rawMax != "" {
		v, err := strconv.Atoi(rawMax)
		if err != nil {
			problems = append(problems, fmt.Sprintf("MAX_WINDOW_MINUTES: must be an integer, got %q", rawMax))
		} else if v <= 0 || v > maxWindowMinutesCap {
			problems = append(problems, fmt.Sprintf("MAX_WINDOW_MINUTES: must be between 1 and %d, got %d", maxWindowMinutesCap, v))
		} else {
			max = v
		}
	}
	c.MaxWindowMinutes = max

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// Redacted returns a copy of the configuration that is safe to expose through
// an MCP resource or a log line. The Kubernetes token is never included.
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"prometheus_url":         c.PrometheusURL,
		"kubernetes_url":         c.KubernetesURL,
		"kubernetes_token":       TokenStatus(c.KubernetesToken),
		"allowed_namespace":      c.AllowedNamespace,
		"allowed_service_prefix": c.AllowedServicePrefix,
		"max_window_minutes":     c.MaxWindowMinutes,
	}
}

// TokenStatus describes the bearer token without revealing it.
func TokenStatus(token string) string {
	if token == "" {
		return "not set (anonymous access)"
	}
	return fmt.Sprintf("set (%d bytes, redacted)", len(token))
}
