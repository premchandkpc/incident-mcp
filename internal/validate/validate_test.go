package validate

import (
	"strings"
	"testing"
	"time"
)

func TestDNSLabel(t *testing.T) {
	valid := []string{"a", "alerts-api", "api1", "a-b-c-1", "0x"}
	for _, s := range valid {
		if err := DNSLabel(s); err != nil {
			t.Errorf("DNSLabel(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{"", "-lead", "trail-", "UPPER", "has space", "dot.ok", strings.Repeat("a", 64), "sla/sh"}
	for _, s := range invalid {
		if err := DNSLabel(s); err == nil {
			t.Errorf("DNSLabel(%q) = nil, want error", s)
		}
	}
}

func TestServiceEnforcesPrefix(t *testing.T) {
	if err := Service("alerts-api", "alerts-"); err != nil {
		t.Errorf("Service(alerts-api) = %v, want nil", err)
	}
	if err := Service("payments-api", "alerts-"); err == nil {
		t.Error("Service(payments-api) with prefix alerts- = nil, want prefix error")
	}
	if err := Service("alerts-api", ""); err != nil {
		t.Errorf("Service with empty prefix = %v, want nil (prefix disabled)", err)
	}
	if err := Service("ALERTS", "alerts-"); err == nil {
		t.Error("Service(uppercase) = nil, want error")
	}
}

func TestServicePrefix(t *testing.T) {
	if err := ServicePrefix("alerts-"); err != nil {
		t.Errorf("ServicePrefix(alerts-) = %v, want nil", err)
	}
	for _, bad := range []string{"", "-bad", "Bad", "alerts_", strings.Repeat("a", 61)} {
		if err := ServicePrefix(bad); err == nil {
			t.Errorf("ServicePrefix(%q) = nil, want error", bad)
		}
	}
}

func TestEndpoint(t *testing.T) {
	valid := []string{
		"https://prometheus.internal",
		"https://kubernetes.default.svc:443",
		"http://127.0.0.1:9090",
		"http://localhost:8080/prometheus",
	}
	for _, s := range valid {
		if err := Endpoint("TEST_URL", s); err != nil {
			t.Errorf("Endpoint(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		"prometheus.internal",           // no scheme
		"ftp://prometheus.internal",     // wrong scheme
		"http://prometheus.internal",    // plaintext, not loopback
		"https://user:pass@kubernetes/", // embedded credentials
		"https://",                      // no host
	}
	for _, s := range invalid {
		if err := Endpoint("TEST_URL", s); err == nil {
			t.Errorf("Endpoint(%q) = nil, want error", s)
		}
	}
}

func TestEndpointRejectsTokenOverPlaintext(t *testing.T) {
	err := Endpoint("KUBERNETES_URL", "http://kubernetes.default.svc")
	if err == nil {
		t.Fatal("Endpoint(plaintext non-loopback) = nil, want error")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("error %q should mention https", err)
	}
}

func TestWindow(t *testing.T) {
	cases := []struct {
		requested, max int
		want           int
		wantClamped    bool
		wantErr        bool
	}{
		{requested: 0, max: 60, want: 15, wantClamped: false},
		{requested: -5, max: 60, want: 15, wantClamped: false},
		{requested: 30, max: 60, want: 30, wantClamped: false},
		{requested: 60, max: 60, want: 60, wantClamped: false},
		{requested: 600, max: 60, want: 60, wantClamped: true},
		{requested: 30, max: 10, want: 10, wantClamped: true},
		{requested: 0, max: 5, want: 5, wantClamped: false}, // default clamped down to max
		{requested: 1, max: 0, wantErr: true},
	}
	for _, c := range cases {
		got, clamped, err := Window(c.requested, c.max)
		if c.wantErr {
			if err == nil {
				t.Errorf("Window(%d, %d) = %d, want error", c.requested, c.max, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Window(%d, %d) unexpected error %v", c.requested, c.max, err)
			continue
		}
		if got != c.want || clamped != c.wantClamped {
			t.Errorf("Window(%d, %d) = (%d, clamped=%v), want (%d, clamped=%v)",
				c.requested, c.max, got, clamped, c.want, c.wantClamped)
		}
	}
}

func TestTimeRangeDefaults(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	start, end, clamped, err := TimeRange("", "", 15, 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if clamped {
		t.Error("default range should not be clamped")
	}
	if !end.Equal(now) {
		t.Errorf("end = %s, want %s", end, now)
	}
	if want := now.Add(-15 * time.Minute); !start.Equal(want) {
		t.Errorf("start = %s, want %s", start, want)
	}
}

func TestTimeRangeClampsToMax(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	requested := now.Add(-6 * time.Hour)
	start, end, clamped, err := TimeRange(requested.Format(time.RFC3339), "", 15, 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clamped {
		t.Fatal("range exceeding max window should be clamped")
	}
	if got := end.Sub(start); got != time.Hour {
		t.Errorf("clamped window = %s, want 1h", got)
	}
	if !start.Equal(now.Add(-time.Hour)) {
		t.Errorf("clamped start = %s, want %s", start, now.Add(-time.Hour))
	}
}

func TestTimeRangeExplicitEnd(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	endStr := "2026-11-17T10:00:00Z"
	startStr := "2026-11-17T09:30:00Z"
	start, end, _, err := TimeRange(startStr, endStr, 15, 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !end.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %s", end)
	}
	if !start.Equal(time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("start = %s", start)
	}
}

func TestTimeRangeRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct{ name, start, end string }{
		{"start after end", "2026-11-17T11:00:00Z", "2026-11-17T10:00:00Z"},
		{"equal", "2026-11-17T10:00:00Z", "2026-11-17T10:00:00Z"},
		{"bad start", "not-a-time", ""},
		{"bad end", "", "not-a-time"},
		{"future end", "", "2027-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		if _, _, _, err := TimeRange(c.start, c.end, 15, 60, now); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

func TestTimeRangeToleratesSmallClockSkew(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	endStr := now.Add(2 * time.Minute).Format(time.RFC3339)
	start, end, clamped, err := TimeRange("", endStr, 15, 60, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !clamped || !end.Equal(now) {
		t.Errorf("end should clamp to now, got end=%s clamped=%v", end, clamped)
	}
	if !start.Equal(now.Add(-15 * time.Minute)) {
		t.Errorf("start = %s", start)
	}
}
