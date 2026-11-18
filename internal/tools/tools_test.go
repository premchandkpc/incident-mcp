package tools

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
)

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func testRegistry() *Registry {
	cfg := config.Config{
		PrometheusURL:        "https://prometheus.internal",
		KubernetesURL:        "https://kubernetes.default.svc",
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	r := NewRegistry(cfg, nil, nil, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return testNow }
	return r
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestResolveDefaults(t *testing.T) {
	r := testRegistry()
	j, err := r.resolve(raw(`{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if j.Service != "alerts-api" {
		t.Errorf("service = %q", j.Service)
	}
	if !j.End.Equal(testNow) {
		t.Errorf("end = %s, want now", j.End)
	}
	if !j.Start.Equal(testNow.Add(-15 * time.Minute)) {
		t.Errorf("start = %s, want now-15m", j.Start)
	}
	if j.Minutes != 15 || j.Clamped {
		t.Errorf("minutes=%d clamped=%v, want 15/false", j.Minutes, j.Clamped)
	}
}

func TestResolveClampsWindowToMaximum(t *testing.T) {
	r := testRegistry()
	j, err := r.resolve(raw(`{"service":"alerts-api","window_minutes":9999}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if j.Minutes != 60 {
		t.Errorf("minutes = %d, want 60", j.Minutes)
	}
	if !j.Clamped {
		t.Error("clamped should be true when the request exceeds the maximum")
	}
	if got := j.End.Sub(j.Start); got != time.Hour {
		t.Errorf("window = %s, want 1h", got)
	}
}

func TestResolveRejectsOutOfScopeAndInvalid(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing service", `{}`, "service"},
		{"empty service", `{"service":""}`, "service"},
		{"prefix violation", `{"service":"payments-api"}`, "prefix"},
		{"not a dns label", `{"service":"Bad Label"}`, "service"},
		{"wrong value type", `{"service":123}`, "invalid arguments"},
		{"malformed json", `{"service":`, "invalid arguments"},
		{"not an object", `["alerts-api"]`, "invalid arguments"},
		{"bad start", `{"service":"alerts-api","start":"yesterday"}`, "start"},
		{"start after end", `{"service":"alerts-api","start":"2026-11-17T11:00:00Z","end":"2026-11-17T10:00:00Z"}`, "earlier"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := r(t).resolve(raw(c.args))
			if err == nil {
				t.Fatalf("resolve(%s) = nil, want error containing %q", c.args, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should contain %q", err, c.want)
			}
		})
	}
}

// r is a tiny accessor so table tests can build a registry per case.
func r(t *testing.T) *Registry { t.Helper(); return testRegistry() }

func TestResolveHonorsExplicitRange(t *testing.T) {
	reg := testRegistry()
	j, err := reg.resolve(raw(`{"service":"alerts-api","start":"2026-11-17T09:00:00Z","end":"2026-11-17T10:00:00Z"}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if j.Minutes != 60 {
		t.Errorf("minutes = %d, want 60", j.Minutes)
	}
	if !j.Start.Equal(time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %s", j.Start)
	}
}

func TestResolveIgnoresEmptyArguments(t *testing.T) {
	reg := testRegistry()
	for _, arg := range []string{"", "null", "  "} {
		if _, err := reg.resolve(raw(arg)); err == nil {
			t.Errorf("resolve(%q) should require a service", arg)
		}
	}
}

func TestAnalyzeFindsSpikeInFlatSeries(t *testing.T) {
	base := testNow
	points := make([]backend.Point, 0, 60)
	for i := 0; i < 60; i++ {
		v := 0.01
		if i == 40 {
			v = 0.9
		}
		points = append(points, backend.Point{T: base.Add(time.Duration(i) * 30 * time.Second), V: v})
	}

	stat, spikes := analyze(points)
	if stat.Points != 60 {
		t.Errorf("points = %d", stat.Points)
	}
	if len(spikes) != 1 {
		t.Fatalf("got %d spikes, want 1 (%+v)", len(spikes), spikes)
	}
	if spikes[0].Value != 0.9 {
		t.Errorf("spike value = %v", spikes[0].Value)
	}
	if stat.SpikeCount != 1 {
		t.Errorf("SpikeCount = %d", stat.SpikeCount)
	}
	if stat.Threshold >= 0.9 {
		t.Errorf("threshold %v should be below the spike", stat.Threshold)
	}
	if stat.Median != 0.01 {
		t.Errorf("median = %v, want 0.01", stat.Median)
	}
}

func TestAnalyzeFlatSeriesHasNoSpikes(t *testing.T) {
	base := testNow
	points := []backend.Point{}
	for i := 0; i < 30; i++ {
		points = append(points, backend.Point{T: base.Add(time.Duration(i) * time.Minute), V: 42})
	}
	stat, spikes := analyze(points)
	if len(spikes) != 0 {
		t.Errorf("flat series produced %d spikes, want 0", len(spikes))
	}
	if stat.StdDev != 0 {
		t.Errorf("stddev = %v, want 0", stat.StdDev)
	}
}

func TestAnalyzeDetectsDenseBursts(t *testing.T) {
	// A mean+3*stddev detector misses this: the outliers inflate the very
	// threshold they are compared against.
	base := testNow
	points := []backend.Point{}
	for i := 0; i < 60; i++ {
		v := 1.0
		if i%3 == 0 {
			v = 2.0
		}
		points = append(points, backend.Point{T: base.Add(time.Duration(i) * time.Minute), V: v})
	}
	stat, spikes := analyze(points)
	if stat.SpikeCount == 0 {
		t.Fatal("dense burst produced no spikes")
	}
	if len(spikes) != stat.SpikeCount && len(spikes) != maxSpikeReport {
		t.Errorf("reported %d spikes, SpikeCount=%d", len(spikes), stat.SpikeCount)
	}
}

func TestAnalyzeCapsSpikeReport(t *testing.T) {
	base := testNow
	points := []backend.Point{}
	for i := 0; i < 100; i++ {
		v := 1.0
		if i%2 == 0 {
			v = 1.5 // 50% above the median, MAD stays 0
		}
		points = append(points, backend.Point{T: base.Add(time.Duration(i) * time.Minute), V: v})
	}
	_, spikes := analyze(points)
	if len(spikes) > maxSpikeReport {
		t.Errorf("reported %d spikes, cap is %d", len(spikes), maxSpikeReport)
	}
	for i := 1; i < len(spikes); i++ {
		if spikes[i].Timestamp < spikes[i-1].Timestamp {
			t.Error("capped spike report should stay chronological")
		}
	}
}

func TestAnalyzeEmptySeries(t *testing.T) {
	stat, spikes := analyze(nil)
	if stat.Points != 0 || len(spikes) != 0 {
		t.Errorf("empty series: stat=%+v spikes=%v", stat, spikes)
	}
}

func TestPodFindingsHealthyPodIsEmpty(t *testing.T) {
	p := backend.Pod{}
	p.Metadata.Name = "alerts-api-1"
	p.Metadata.CreationTimestamp = testNow.Add(-time.Hour)
	p.Status.Phase = "Running"
	p.Status.Conditions = []backend.PodCondition{{Type: "Ready", Status: "True"}}
	p.Status.ContainerStatuses = []backend.ContainerStatus{{Name: "app", Ready: true, RestartCount: 1}}

	if findings := podFindings(p, testNow); len(findings) != 0 {
		t.Errorf("healthy pod findings = %v, want none", findings)
	}
}

func TestPodFindingsCrashLoop(t *testing.T) {
	p := backend.Pod{}
	p.Metadata.Name = "alerts-api-2"
	p.Metadata.CreationTimestamp = testNow.Add(-time.Hour)
	p.Status.Phase = "Running"
	p.Status.Conditions = []backend.PodCondition{{Type: "Ready", Status: "False", Reason: "ContainersNotReady"}}
	p.Status.ContainerStatuses = []backend.ContainerStatus{{
		Name:         "app",
		Ready:        false,
		RestartCount: 9,
		State: backend.ContainerState{
			Waiting: &struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}{Reason: "CrashLoopBackOff", Message: "back-off 40s restarting failed container=app"},
		},
		LastState: backend.ContainerState{
			Terminated: &struct {
				Reason     string    `json:"reason"`
				Message    string    `json:"message"`
				ExitCode   int       `json:"exitCode"`
				FinishedAt time.Time `json:"finishedAt"`
			}{Reason: "Error", ExitCode: 1, FinishedAt: testNow.Add(-time.Minute)},
		},
	}}

	findings := podFindings(p, testNow)
	joined := strings.Join(findings, " | ")
	for _, want := range []string{"CrashLoopBackOff", "exit 1", "restarted 9 times", "readiness"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings %q should mention %q", joined, want)
		}
	}
}

func TestPodFindingsFailedAndPending(t *testing.T) {
	failed := backend.Pod{}
	failed.Metadata.Name = "alerts-api-f"
	failed.Status.Phase = "Failed"
	failed.Status.Message = "Pod Failed (evicted)"
	if got := strings.Join(podFindings(failed, testNow), " "); !strings.Contains(got, "Failed") {
		t.Errorf("failed pod findings = %q", got)
	}

	pending := backend.Pod{}
	pending.Metadata.Name = "alerts-api-p"
	pending.Metadata.CreationTimestamp = testNow.Add(-10 * time.Minute)
	pending.Status.Phase = "Pending"
	if got := strings.Join(podFindings(pending, testNow), " "); !strings.Contains(got, "pending for") {
		t.Errorf("pending pod findings = %q", got)
	}

	fresh := backend.Pod{}
	fresh.Metadata.Name = "alerts-api-n"
	fresh.Metadata.CreationTimestamp = testNow.Add(-10 * time.Second)
	fresh.Status.Phase = "Pending"
	if got := podFindings(fresh, testNow); len(got) != 0 {
		t.Errorf("fresh pending pod findings = %v, want none", got)
	}
}

func TestPodFindingsBoundsMessageLength(t *testing.T) {
	p := backend.Pod{}
	p.Metadata.Name = "alerts-api-long"
	p.Status.Phase = "Failed"
	p.Status.Message = strings.Repeat("x", 5000)

	findings := podFindings(p, testNow)
	if len(findings) != 1 {
		t.Fatalf("findings = %v", findings)
	}
	if len(findings[0]) > 300 {
		t.Errorf("finding is %d bytes; long cluster messages must be truncated", len(findings[0]))
	}
}

func TestRegistryCallUnknownTool(t *testing.T) {
	reg := testRegistry()
	if _, err := reg.Call(context.Background(), "does_not_exist", nil); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}

func TestRegistryListExposesSixTools(t *testing.T) {
	reg := testRegistry()
	list := reg.List()
	if len(list) != 6 {
		t.Fatalf("got %d tools, want 6", len(list))
	}
	names := map[string]bool{}
	for _, tool := range list {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s schema type = %v", tool.Name, tool.InputSchema["type"])
		}
		if tool.Handler == nil {
			t.Errorf("tool %s has no handler", tool.Name)
		}
	}
	for _, want := range []string{
		"investigate_incident", "get_error_spikes", "get_pod_failures",
		"get_recent_deployments", "get_deploy_history", "get_alerts",
	} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

// everyTool lists every tool that takes a service argument, so scope checks
// can be asserted across the whole surface rather than a hard-coded subset.
func everyTool() []string {
	names := make([]string, 0, 6)
	for _, t := range testRegistry().List() {
		names = append(names, t.Name)
	}
	return names
}

func TestCallRejectsOutOfScopeServiceBeforeTouchingBackends(t *testing.T) {
	reg := testRegistry() // backends are nil: any backend access would panic
	for _, name := range everyTool() {
		res, err := reg.Call(context.Background(), name, raw(`{"service":"other-team-api"}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("%s: out-of-scope service should be an error result", name)
		}
	}
}

func TestCallWithoutBackendsFailsCleanly(t *testing.T) {
	reg := testRegistry()
	for _, name := range everyTool() {
		if name == "investigate_incident" {
			continue // asserted separately; it aggregates the other sections
		}
		res, err := reg.Call(context.Background(), name, raw(`{"service":"alerts-api"}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("%s: missing backend should surface as a tool error", name)
		}
		if len(res.Content) != 1 || res.Content[0].Type != "text" {
			t.Errorf("%s: unexpected content %+v", name, res.Content)
		}
	}
}

func TestStepForStaysBounded(t *testing.T) {
	start := testNow.Add(-60 * time.Minute)
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{60 * time.Minute, 30 * time.Second},
		{15 * time.Minute, 15 * time.Second},
		{time.Minute, 15 * time.Second},
	}
	for _, c := range cases {
		got := stepFor(testNow.Add(-c.window), testNow)
		if got != c.want {
			t.Errorf("stepFor(%s) = %s, want %s", c.window, got, c.want)
		}
	}
	if got := stepFor(start, start); got != 15*time.Second {
		t.Errorf("degenerate range step = %s", got)
	}
}

func TestSeriesJSONTruncates(t *testing.T) {
	points := []backend.Point{}
	for i := 0; i < maxSeriesPoints+50; i++ {
		points = append(points, backend.Point{T: testNow.Add(time.Duration(i) * time.Second), V: float64(i)})
	}
	out, truncated := seriesJSON(points)
	if !truncated {
		t.Error("expected truncation")
	}
	if len(out) != maxSeriesPoints {
		t.Errorf("got %d points, want %d", len(out), maxSeriesPoints)
	}
	// The most recent samples must survive.
	last := out[len(out)-1]
	if last["value"].(float64) != float64(maxSeriesPoints+49) {
		t.Errorf("truncation should keep the newest samples, last = %v", last)
	}
}
