package config

import (
	"strings"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"PROMETHEUS_URL":         "https://prometheus.internal",
		"KUBERNETES_URL":         "https://kubernetes.default.svc",
		"KUBERNETES_TOKEN":       "eyJhbGciOi.secret-token-value",
		"ALLOWED_NAMESPACE":      "production",
		"ALLOWED_SERVICE_PREFIX": "alerts-",
		"MAX_WINDOW_MINUTES":     "60",
	}
}

func TestFromEnvValid(t *testing.T) {
	cfg, err := FromEnv(envFrom(validEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AllowedNamespace != "production" || cfg.AllowedServicePrefix != "alerts-" {
		t.Errorf("unexpected scope: %+v", cfg)
	}
	if cfg.MaxWindowMinutes != 60 {
		t.Errorf("MaxWindowMinutes = %d, want 60", cfg.MaxWindowMinutes)
	}
	if cfg.KubernetesToken != "eyJhbGciOi.secret-token-value" {
		t.Errorf("token should be loaded verbatim into memory, got %q", cfg.KubernetesToken)
	}
}

func TestFromEnvDefaultsMaxWindow(t *testing.T) {
	env := validEnv()
	delete(env, "MAX_WINDOW_MINUTES")
	cfg, err := FromEnv(envFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxWindowMinutes != DefaultMaxWindowMinutes {
		t.Errorf("MaxWindowMinutes = %d, want %d", cfg.MaxWindowMinutes, DefaultMaxWindowMinutes)
	}
}

func TestFromEnvRejectsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing prometheus url", func(e map[string]string) { delete(e, "PROMETHEUS_URL") }, "PROMETHEUS_URL"},
		{"missing kubernetes url", func(e map[string]string) { delete(e, "KUBERNETES_URL") }, "KUBERNETES_URL"},
		{"missing namespace", func(e map[string]string) { delete(e, "ALLOWED_NAMESPACE") }, "ALLOWED_NAMESPACE"},
		{"missing service prefix", func(e map[string]string) { delete(e, "ALLOWED_SERVICE_PREFIX") }, "ALLOWED_SERVICE_PREFIX"},
		{"plaintext kubernetes", func(e map[string]string) { e["KUBERNETES_URL"] = "http://kubernetes.default.svc" }, "https"},
		{"credentials in url", func(e map[string]string) { e["PROMETHEUS_URL"] = "https://user:pass@prom.internal" }, "credentials"},
		{"bad namespace", func(e map[string]string) { e["ALLOWED_NAMESPACE"] = "Production!" }, "ALLOWED_NAMESPACE"},
		{"bad max window", func(e map[string]string) { e["MAX_WINDOW_MINUTES"] = "60m" }, "MAX_WINDOW_MINUTES"},
		{"zero max window", func(e map[string]string) { e["MAX_WINDOW_MINUTES"] = "0" }, "MAX_WINDOW_MINUTES"},
		{"huge max window", func(e map[string]string) { e["MAX_WINDOW_MINUTES"] = "999999" }, "MAX_WINDOW_MINUTES"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := validEnv()
			c.mutate(env)
			_, err := FromEnv(envFrom(env))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should contain %q", err, c.want)
			}
		})
	}
}

func TestRedactedNeverExposesToken(t *testing.T) {
	cfg, err := FromEnv(envFrom(validEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	red := cfg.Redacted()
	raw := fmtAll(red)
	if strings.Contains(raw, "secret-token-value") {
		t.Fatalf("redacted config leaked the token: %s", raw)
	}
	if got, _ := red["kubernetes_token"].(string); !strings.Contains(got, "redacted") {
		t.Errorf("kubernetes_token = %q, want a redacted marker", got)
	}
	if got, _ := red["allowed_namespace"].(string); got != "production" {
		t.Errorf("allowed_namespace = %q", got)
	}
}

func TestTokenStatus(t *testing.T) {
	if got := TokenStatus(""); !strings.Contains(got, "not set") {
		t.Errorf("TokenStatus(\"\") = %q", got)
	}
	if got := TokenStatus("abc"); strings.Contains(got, "abc") {
		t.Errorf("TokenStatus leaked the token: %q", got)
	}
}

// fmtAll renders a map deterministically enough for substring assertions.
func fmtAll(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(toString(v))
		b.WriteString(";")
	}
	return b.String()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
