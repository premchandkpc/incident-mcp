package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
)

// newTestHarness builds a registry wired to local mock backends. failTotal
// makes Prometheus reject only the secondary (request-rate) query, which is
// the partial-failure path worth exercising.
func newTestHarness(t *testing.T, failTotal bool, podsJSON, deploymentsJSON string) *Registry {
	t.Helper()

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		startSecs, err := strconv.ParseFloat(q.Get("start"), 64)
		if err != nil {
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		start := time.Unix(int64(startSecs), 0).UTC()
		stepSecs, _ := strconv.ParseFloat(q.Get("step"), 64)
		step := time.Duration(stepSecs * float64(time.Second))
		if step <= 0 {
			step = time.Minute
		}
		errorSeries := strings.Contains(q.Get("query"), `code=~"5.."`)
		if failTotal && !errorSeries {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream blew up")
			return
		}

		base := 10.0
		if errorSeries {
			base = 0.01
		}
		var sb strings.Builder
		sb.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[`)
		for i := 0; i <= 60; i++ {
			v := base
			if errorSeries && i == 40 {
				v = 0.9
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `["%d","%g"]`, start.Add(time.Duration(i)*step).Unix(), v)
		}
		sb.WriteString(`]}]}}`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sb.String())
	}))
	t.Cleanup(prom.Close)

	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			_, _ = io.WriteString(w, podsJSON)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			_, _ = io.WriteString(w, deploymentsJSON)
		case strings.HasSuffix(r.URL.Path, "/replicasets"):
			_, _ = io.WriteString(w, harnessReplicaSets)
		case strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = io.WriteString(w, harnessEvents)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kube.Close)

	cfg := config.Config{
		PrometheusURL:        prom.URL,
		KubernetesURL:        kube.URL,
		KubernetesToken:      "not-a-real-token",
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	p, err := backend.NewPrometheus(cfg.PrometheusURL, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	k, err := backend.NewKubernetes(cfg.KubernetesURL, cfg.KubernetesToken, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("kubernetes: %v", err)
	}
	r := NewRegistry(cfg, p, k, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return testNow }
	return r
}

const harnessPods = `{"items":[
  {"metadata":{"name":"alerts-api-a","namespace":"production","creationTimestamp":"2026-11-17T08:00:00Z"},
   "spec":{"containers":[{"name":"app","image":"img:1"}]},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],
     "containerStatuses":[{"name":"app","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-11-17T08:00:05Z"}}}]}}]}`

const harnessReplicaSets = `{"items":[
  {"metadata":{"name":"alerts-api-7f7d9-x1","namespace":"production","creationTimestamp":"2026-11-17T11:30:00Z",
     "annotations":{"deployment.kubernetes.io/revision":"15"},"labels":{"app":"alerts-api"}},
   "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:3"}]}}},
   "status":{"replicas":2,"readyReplicas":2,"availableReplicas":2}},
  {"metadata":{"name":"alerts-api-6f7d9-a2","namespace":"production","creationTimestamp":"2026-11-10T10:00:00Z",
     "annotations":{"deployment.kubernetes.io/revision":"14"},"labels":{"app":"alerts-api"}},
   "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:2"}]}}},
   "status":{"replicas":0,"readyReplicas":0,"availableReplicas":0}}]}`

const harnessEvents = `{"items":[
  {"metadata":{"name":"alerts-api-7f7d9-x1.17a1b2","namespace":"production","creationTimestamp":"2026-11-17T11:31:00Z"},
   "type":"Normal","reason":"Scheduled","count":1,
   "firstTimestamp":"2026-11-17T11:31:00Z","lastTimestamp":"2026-11-17T11:31:00Z",
   "involvedObject":{"kind":"Pod","name":"alerts-api-7f7d9-x1","uid":"u1"},
   "message":"Successfully assigned alerts-api/alerts-api-7f7d9-x1 to node-a"},
  {"metadata":{"name":"alerts-api-7f7d9-x1.17a1b3","namespace":"production","creationTimestamp":"2026-11-17T11:32:00Z"},
   "type":"Warning","reason":"BackOff","count":4,
   "firstTimestamp":"2026-11-17T11:32:00Z","lastTimestamp":"2026-11-17T11:35:00Z",
   "involvedObject":{"kind":"Pod","name":"alerts-api-7f7d9-x1","uid":"u1"},
   "message":"Back-off 40s restarting failed container=app"},
  {"metadata":{"name":"other-team-api-z.1","namespace":"production","creationTimestamp":"2026-11-17T11:33:00Z"},
   "type":"Warning","reason":"BackOff","count":9,
   "firstTimestamp":"2026-11-17T11:33:00Z","lastTimestamp":"2026-11-17T11:33:00Z",
   "involvedObject":{"kind":"Pod","name":"other-team-api-z","uid":"u2"},
   "message":"must not appear: another service's event"}]}`

const harnessDeployments = `{"items":[
  {"metadata":{"name":"alerts-api","namespace":"production","creationTimestamp":"2026-11-10T10:00:00Z",
     "annotations":{"deployment.kubernetes.io/revision":"14"}},
   "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:2"}]}}},
   "status":{"replicas":2,"updatedReplicas":2,"readyReplicas":1,"availableReplicas":1,
     "conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}}]}`

func mustJob(t *testing.T, reg *Registry, args string) job {
	t.Helper()
	j, err := reg.resolve(raw(args))
	if err != nil {
		t.Fatalf("resolve(%s): %v", args, err)
	}
	return j
}

func TestErrorSpikesEvidenceWithBrokenSecondaryQuery(t *testing.T) {
	reg := newTestHarness(t, true, harnessPods, harnessDeployments)
	j := mustJob(t, reg, `{"service":"alerts-api","window_minutes":30}`)

	ev, err := reg.errorSpikesEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("primary query succeeded, evidence must be returned: %v", err)
	}
	partial := ev["partial_failures"].([]string)
	// The secondary request-rate query must be reported. Latency queries are
	// also secondary and fail here too; assert on presence rather than an
	// exact count so adding a secondary query does not silently pass.
	found := false
	for _, p := range partial {
		if strings.Contains(p, "request-rate query failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("partial_failures = %v", partial)
	}
	if _, ok := ev["peak_error_ratio"]; ok {
		t.Error("peak_error_ratio should be absent when the total query fails")
	}
	if len(ev["spikes"].([]Spike)) != 1 {
		t.Errorf("spikes = %v", ev["spikes"])
	}
	if _, ok := ev["request_rate_summary"]; ok {
		t.Error("request_rate_summary should be absent when the total query fails")
	}
}

func TestErrorSpikesEvidencePrimaryQueryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "prometheus is down")
	}))
	defer srv.Close()

	cfg := config.Config{
		PrometheusURL:        srv.URL,
		KubernetesURL:        "https://kubernetes.default.svc",
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	p, err := backend.NewPrometheus(cfg.PrometheusURL, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	reg := NewRegistry(cfg, p, nil, log.New(io.Discard, "", 0))
	reg.now = func() time.Time { return testNow }

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	if _, err := reg.errorSpikesEvidence(context.Background(), j); err == nil {
		t.Fatal("expected an error when the primary query fails")
	}

	// The tool wrapper must surface it as an MCP tool error, not a panic.
	res, err := reg.Call(context.Background(), "get_error_spikes", raw(`{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.IsError {
		t.Fatal("down Prometheus must produce isError=true")
	}
	if !strings.Contains(res.Content[0].Text, "prometheus is down") &&
		!strings.Contains(res.Content[0].Text, "502") {
		t.Errorf("tool error should explain the failure: %s", res.Content[0].Text)
	}
}

func TestPodEvidenceEmptySelectorIsPartialFailure(t *testing.T) {
	reg := newTestHarness(t, false, `{"items":[]}`, harnessDeployments)
	j := mustJob(t, reg, `{"service":"alerts-api"}`)

	ev, err := reg.podEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("podEvidence: %v", err)
	}
	partial := ev["partial_failures"].([]string)
	if len(partial) != 1 || !strings.Contains(partial[0], "no pods matched") {
		t.Errorf("partial_failures = %v", partial)
	}
	if ev["pod_count"].(int) != 0 {
		t.Errorf("pod_count = %v", ev["pod_count"])
	}
	if ev["selector"] != "app=alerts-api" || ev["namespace"] != "production" {
		t.Errorf("scope = %v %v", ev["namespace"], ev["selector"])
	}
	if len(ev["pods"].([]map[string]any)) != 0 {
		t.Errorf("pods should be an empty list, not nil")
	}
}

func TestPodEvidenceReportsHealthyAndDegraded(t *testing.T) {
	reg := newTestHarness(t, false, harnessPods, harnessDeployments)
	j := mustJob(t, reg, `{"service":"alerts-api"}`)

	ev, err := reg.podEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("podEvidence: %v", err)
	}
	if ev["healthy_pods"].(int) != 1 || ev["degraded_pods"].(int) != 0 {
		t.Errorf("counts = healthy %v degraded %v", ev["healthy_pods"], ev["degraded_pods"])
	}
	pods := ev["pods"].([]map[string]any)
	if pods[0]["ready"] != true {
		t.Errorf("ready = %v", pods[0]["ready"])
	}
	if pods[0]["age"] == "" || pods[0]["created"] == "" {
		t.Errorf("age/created missing: %+v", pods[0])
	}
}

func TestDeploymentEvidenceFlagsIncompleteRollout(t *testing.T) {
	reg := newTestHarness(t, false, harnessPods, harnessDeployments)
	j := mustJob(t, reg, `{"service":"alerts-api"}`)

	ev, err := reg.deploymentEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("deploymentEvidence: %v", err)
	}
	if ev["deployment_count"].(int) != 1 {
		t.Fatalf("deployment_count = %v", ev["deployment_count"])
	}
	dep := ev["deployments"].([]map[string]any)[0]
	replicas := dep["replicas"].(map[string]any)
	if replicas["desired"].(int) != 2 || replicas["ready"].(int) != 1 {
		t.Errorf("replicas = %v", replicas)
	}
	if dep["rollout_complete"].(bool) {
		t.Error("1/2 ready must not be reported as a complete rollout")
	}
	obs := ev["observations"].([]string)
	if len(obs) != 1 || !strings.Contains(obs[0], "not fully rolled out") {
		t.Errorf("observations = %v", obs)
	}
	if dep["revision"] != "14" {
		t.Errorf("revision = %v", dep["revision"])
	}
}

func TestIncidentEvidenceAllBackendsDown(t *testing.T) {
	reg := testRegistry() // nil backends for every section
	j := mustJob(t, reg, `{"service":"alerts-api"}`)

	ev, allFailed := reg.incidentEvidence(context.Background(), j)
	if !allFailed {
		t.Fatal("allFailed should be true when every section fails")
	}
	failed := ev["failed_sections"].([]string)
	if len(failed) != 3 {
		t.Fatalf("failed_sections = %v", failed)
	}
	evidence := ev["evidence"].(map[string]any)
	for _, key := range []string{"error_spikes", "pod_failures", "recent_deployments"} {
		if evidence[key] != nil {
			t.Errorf("evidence.%s should be null when it failed", key)
		}
	}
	partial := ev["partial_failures"].([]string)
	if len(partial) != 3 {
		t.Fatalf("partial_failures = %v", partial)
	}
	if !strings.Contains(partial[0], "error_spikes:") {
		t.Errorf("partial failures should name their section: %v", partial)
	}
}

func TestIncidentEvidencePartialBackendFailure(t *testing.T) {
	reg := newTestHarness(t, false, harnessPods, harnessDeployments)
	// Break Prometheus only.
	reg.prom = nil

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	ev, allFailed := reg.incidentEvidence(context.Background(), j)
	if allFailed {
		t.Fatal("two of three sections succeeded; allFailed must be false")
	}
	failed := ev["failed_sections"].([]string)
	if len(failed) != 1 || failed[0] != "error_spikes" {
		t.Fatalf("failed_sections = %v", failed)
	}
	evidence := ev["evidence"].(map[string]any)
	if evidence["pod_failures"] == nil || evidence["recent_deployments"] == nil {
		t.Errorf("healthy sections must still be returned: %v", keysOf(evidence))
	}
	res, err := reg.Call(context.Background(), "investigate_incident", raw(`{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Error("a partial investigation is not a tool error")
	}
	if !strings.Contains(res.Content[0].Text, "error_spikes") {
		t.Errorf("output should disclose the failed section: %s", firstLines(res.Content[0].Text))
	}
}

func TestIncidentEvidenceIncludesWindowAndNotes(t *testing.T) {
	reg := newTestHarness(t, false, harnessPods, harnessDeployments)
	j := mustJob(t, reg, `{"service":"alerts-api","window_minutes":9999}`)

	ev, _ := reg.incidentEvidence(context.Background(), j)
	window := ev["window"].(map[string]any)
	if window["minutes"].(int) != 60 || window["clamped_to_max_window"] != true {
		t.Errorf("window = %v", window)
	}
	notes := fmt.Sprint(ev["notes"])
	for _, want := range []string{"illustrative", "app=<service>", "not historical", "confidence scoring", "redaction"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes should mention %q: %s", want, notes)
		}
	}
	if ev["service"] != "alerts-api" || ev["namespace"] != "production" {
		t.Errorf("identity fields wrong: %v %v", ev["service"], ev["namespace"])
	}
}

func TestToolsReturnJSONTextContent(t *testing.T) {
	reg := newTestHarness(t, false, harnessPods, harnessDeployments)
	for _, name := range []string{"get_error_spikes", "get_pod_failures", "get_recent_deployments", "investigate_incident"} {
		res, err := reg.Call(context.Background(), name, raw(`{"service":"alerts-api"}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s: unexpected error: %s", name, res.Content[0].Text)
		}
		if len(res.Content) != 1 || res.Content[0].Type != "text" {
			t.Fatalf("%s: content = %+v", name, res.Content)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
			t.Fatalf("%s: output is not JSON: %v", name, err)
		}
		if _, ok := payload["service"]; !ok {
			t.Errorf("%s: output missing service", name)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstLines(s string) string {
	lines := strings.SplitN(s, "\n", 5)
	return strings.Join(lines, "\n")
}
