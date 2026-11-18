package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
)

// TestListToolsPageAndReportTruncation proves the bounded-output rule: a list
// tool must send a limit upstream and surface has_more instead of silently
// returning a partial answer.
func TestListToolsPageAndReportTruncation(t *testing.T) {
	var gotLimit, gotContinue string
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		gotContinue = r.URL.Query().Get("continue")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			fmt.Fprint(w, `{"metadata":{"continue":"tok-2"},"items":[]}`)
		case strings.HasSuffix(r.URL.Path, "/replicasets"):
			fmt.Fprint(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer kube.Close()

	reg := registryWithKube(t, kube.URL)
	got, err := reg.Call(context.Background(), "get_pod_failures", raw(`{"service":"alerts-api","limit":5}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotLimit != "5" {
		t.Errorf("limit sent upstream = %q, want 5 — an unbounded list defeats bounded output", gotLimit)
	}
	if gotContinue != "" {
		t.Errorf("first page must not send a continue token, got %q", gotContinue)
	}
	ev := decodeResult(t, got)
	page := ev["page"].(map[string]any)
	if page["has_more"] != true {
		t.Errorf("page = %v, want has_more=true", page)
	}
	if page["next_continue"] != "tok-2" {
		t.Errorf("page = %v, want next_continue", page)
	}
	partial := fmt.Sprint(ev["partial_failures"])
	if !strings.Contains(partial, "truncated") {
		t.Errorf("a truncated list must be disclosed: %v", partial)
	}
}

// TestLimitIsCappedUpstream proves a caller cannot ask for an unbounded page.
func TestLimitIsCappedUpstream(t *testing.T) {
	var gotLimit string
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer kube.Close()

	reg := registryWithKube(t, kube.URL)
	if _, err := reg.Call(context.Background(), "get_pod_failures", raw(`{"service":"alerts-api","limit":99999}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotLimit != fmt.Sprint(backend.MaxListLimit) {
		t.Errorf("limit sent upstream = %q, want the cap %d", gotLimit, backend.MaxListLimit)
	}
}

// TestRejectsMaliciousContinueToken: the continue token is echoed back to an
// upstream API server, so it must be constrained to the character set a
// Kubernetes continue token actually uses.
func TestRejectsMaliciousContinueToken(t *testing.T) {
	reg := testRegistry()
	for _, tok := range []string{
		"../../etc/passwd",
		"tok&state=all",
		"tok\nInjected: yes",
		strings.Repeat("a", 5000),
	} {
		if _, err := reg.resolve(raw(fmt.Sprintf(`{"service":"alerts-api","continue":%q}`, tok))); err == nil {
			t.Errorf("continue token %q should have been rejected", tok)
		}
	}
	// A well-formed token must be accepted.
	if _, err := reg.resolve(raw(`{"service":"alerts-api","continue":"eyJwYWdlIjoyfQ=="}`)); err != nil {
		t.Errorf("valid base64 continue token rejected: %v", err)
	}
}

// TestHistoryWithoutRevisionAnnotationsMarksOnlyNewestCurrent: clusters that
// omit the revision annotation must still yield a usable "current" answer,
// and only one entry may ever be current.
func TestHistoryWithoutRevisionAnnotationsMarksOnlyNewestCurrent(t *testing.T) {
	reg := registryWithKube(t, kubefix(t, "replicasets", `{"items":[
      {"metadata":{"name":"older","creationTimestamp":"2026-11-01T00:00:00Z"},
       "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"app","image":"img:1"}]}}},
       "status":{"readyReplicas":1}},
      {"metadata":{"name":"newer","creationTimestamp":"2026-11-17T00:00:00Z"},
       "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"app","image":"img:2"}]}}},
       "status":{"readyReplicas":1}}]}`))

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	ev, err := reg.historyEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("historyEvidence: %v", err)
	}
	rollouts := ev["rollout_history"].([]rolloutEntry)
	if len(rollouts) != 2 {
		t.Fatalf("got %d rollouts, want 2", len(rollouts))
	}
	current := 0
	for _, e := range rollouts {
		if e.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("%d entries marked current, want exactly 1", current)
	}
	if rollouts[0].Name != "newer" {
		t.Errorf("newest should sort first, got %q", rollouts[0].Name)
	}
	if !rollouts[0].Current {
		t.Error("without revision annotations the newest entry must be current")
	}
}

// TestHistoryEvidenceOrdersRevisionsNewestFirst is the core of "what changed
// recently": revisions must be newest-first with the current one identified.
func TestHistoryEvidenceOrdersRevisionsNewestFirst(t *testing.T) {
	reg := registryWithKube(t, kubefix(t, "replicasets", `{"items":[
      {"metadata":{"name":"old","annotations":{"deployment.kubernetes.io/revision":"3"},
         "creationTimestamp":"2026-11-01T00:00:00Z"},
       "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"app","image":"img:old"}]}}},
       "status":{"readyReplicas":0}},
      {"metadata":{"name":"new","annotations":{"deployment.kubernetes.io/revision":"9"},
         "creationTimestamp":"2026-11-17T00:00:00Z"},
       "spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:new"}]}}},
       "status":{"readyReplicas":2}},
      {"metadata":{"name":"no-revision","creationTimestamp":"2026-11-10T00:00:00Z"},
       "spec":{"template":{"spec":{"containers":[{"name":"app","image":"img:mid"}]}}},
       "status":{}}]}`))

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	ev, err := reg.historyEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("historyEvidence: %v", err)
	}
	rollouts := ev["rollout_history"].([]rolloutEntry)
	if len(rollouts) != 3 {
		t.Fatalf("got %d rollouts, want 3", len(rollouts))
	}
	if rollouts[0].Revision != 9 || rollouts[1].Revision != 3 {
		t.Errorf("order = %d, %d; want 9 then 3", rollouts[0].Revision, rollouts[1].Revision)
	}
	if !rollouts[0].Current {
		t.Error("the highest revision must be marked current")
	}
	if rollouts[1].Current || rollouts[2].Current {
		t.Error("only the highest revision may be current")
	}
	if rollouts[0].Images[0] != "img:new" {
		t.Errorf("images = %v", rollouts[0].Images)
	}
	// A ReplicaSet without the annotation is kept rather than dropped.
	if rollouts[2].Revision != 0 || len(rollouts[2].Images) == 0 {
		t.Errorf("unannotated ReplicaSet should be retained: %+v", rollouts[2])
	}
}

// TestHistoryEvidenceScopesEventsToObjectNames proves events are filtered to
// this service — both by prefix and by the requested window.
func TestHistoryEvidenceScopesEventsToObjectNames(t *testing.T) {
	reg := registryWithKube(t, kubefixMap(t, map[string]string{
		"replicasets": `{"items":[{"metadata":{"name":"alerts-api-abc","annotations":{"deployment.kubernetes.io/revision":"4"},"creationTimestamp":"2026-11-17T11:40:00Z"},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:4"}]}}},"status":{"readyReplicas":2}}]}`,
		"events": `{"items":[
      {"metadata":{"name":"mine.1","creationTimestamp":"2026-11-17T11:50:00Z"},
       "type":"Warning","reason":"BackOff","count":2,
       "firstTimestamp":"2026-11-17T11:50:00Z","lastTimestamp":"2026-11-17T11:50:00Z",
       "involvedObject":{"kind":"Pod","name":"alerts-api-abc"},"message":"boom"},
      {"metadata":{"name":"prefix.1","creationTimestamp":"2026-11-17T11:51:00Z"},
       "type":"Warning","reason":"OOMKilled","count":1,
       "firstTimestamp":"2026-11-17T11:51:00Z","lastTimestamp":"2026-11-17T11:51:00Z",
       "involvedObject":{"kind":"ReplicaSet","name":"alerts-api-abc-def"},"message":"oom"},
      {"metadata":{"name":"sibling.1","creationTimestamp":"2026-11-17T11:52:00Z"},
       "type":"Warning","reason":"BackOff","count":9,
       "firstTimestamp":"2026-11-17T11:52:00Z","lastTimestamp":"2026-11-17T11:52:00Z",
       "involvedObject":{"kind":"Pod","name":"alerts-api-v2-abc"},"message":"sibling service"},
      {"metadata":{"name":"other.1","creationTimestamp":"2026-11-17T11:53:00Z"},
       "type":"Warning","reason":"BackOff","count":9,
       "firstTimestamp":"2026-11-17T11:53:00Z","lastTimestamp":"2026-11-17T11:53:00Z",
       "involvedObject":{"kind":"Pod","name":"payments-api-abc"},"message":"other team"},
      {"metadata":{"name":"stale.1","creationTimestamp":"2026-11-16T11:50:00Z"},
       "type":"Warning","reason":"BackOff","count":1,
       "firstTimestamp":"2026-11-16T11:50:00Z","lastTimestamp":"2026-11-16T11:50:00Z",
       "involvedObject":{"kind":"Pod","name":"alerts-api-old"},"message":"outside window"}]}`,
	}))

	j := mustJob(t, reg, `{"service":"alerts-api","window_minutes":30}`)
	ev, err := reg.historyEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("historyEvidence: %v", err)
	}
	events := ev["events"].([]map[string]any)
	reasons := make([]string, 0, len(events))
	for _, e := range events {
		reasons = append(reasons, fmt.Sprint(e["reason"]))
	}
	joined := strings.Join(reasons, ",")
	if !strings.Contains(joined, "BackOff") || !strings.Contains(joined, "OOMKilled") {
		t.Errorf("in-window events missing: %v", reasons)
	}
	if strings.Contains(fmt.Sprint(events), "payments-api") {
		t.Error("leaked another team's event")
	}
	if strings.Contains(fmt.Sprint(events), "alerts-api-v2") {
		t.Error("prefix match must require a dash boundary: alerts-api-v2 is not alerts-api")
	}
	if strings.Contains(fmt.Sprint(events), "outside window") || len(events) != 2 {
		t.Errorf("window filter failed, got %d events: %v", len(events), events)
	}
}

// TestHistoryEvidenceDeduplicatesEvents: the same observation arriving from
// both kind queries must appear once.
func TestHistoryEvidenceDeduplicatesEvents(t *testing.T) {
	fixture := `{"items":[
      {"metadata":{"name":"dup.1","uid":"same-uid","creationTimestamp":"2026-11-17T11:50:00Z"},
       "type":"Warning","reason":"BackOff","count":2,
       "firstTimestamp":"2026-11-17T11:50:00Z","lastTimestamp":"2026-11-17T11:50:00Z",
       "involvedObject":{"kind":"Pod","name":"alerts-api-abc"},"message":"boom"}]}`
	reg := registryWithKube(t, kubefixMap(t, map[string]string{
		"replicasets": `{"items":[{"metadata":{"name":"alerts-api-abc","annotations":{"deployment.kubernetes.io/revision":"4"},"creationTimestamp":"2026-11-17T11:40:00Z"},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"img:4"}]}}},"status":{"readyReplicas":2}}]}`,
		"events":      fixture,
	}))

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	ev, err := reg.historyEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("historyEvidence: %v", err)
	}
	if got := len(ev["events"].([]map[string]any)); got != 1 {
		t.Errorf("got %d events, want 1 (the two kind queries overlap)", got)
	}
}

// TestAlertsScopesToServiceAndSortsFiringFirst covers the alert tool's two
// guarantees: it stays inside the service scope, and firing outranks pending.
func TestAlertsScopesToServiceAndSortsFiringFirst(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			t.Errorf("path = %s, want /api/v1/alerts", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("state"); got != "" {
			t.Errorf("unfiltered call should not send state, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"alerts":[
          {"labels":{"alertname":"Mine","service":"alerts-api"},"state":"pending",
           "activeAt":"2026-11-17T11:00:00Z","value":1,"annotations":{"summary":"s"}},
          {"labels":{"alertname":"Theirs","service":"payments-api"},"state":"firing",
           "activeAt":"2026-11-17T11:00:00Z","value":1},
          {"labels":{"alertname":"ClusterWide"},"state":"firing",
           "activeAt":"2026-11-17T11:01:00Z","value":1},
          {"labels":{"alertname":"MineFiring","service":"alerts-api"},"state":"firing",
           "activeAt":"2026-11-17T11:02:00Z","value":1}]}}`)
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	ev, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("alertEvidence: %v", err)
	}
	alerts := ev["alerts"].([]map[string]any)
	if len(alerts) != 3 {
		t.Fatalf("got %d alerts, want 3 (the other service's must be dropped): %v", len(alerts), alerts)
	}
	if alerts[0]["state"] != "firing" || alerts[1]["state"] != "firing" {
		t.Errorf("firing alerts must sort before pending: %v then %v", alerts[0]["state"], alerts[1]["state"])
	}
	if alerts[0]["name"] != "MineFiring" {
		t.Errorf("newest firing alert should lead, got %v", alerts[0]["name"])
	}
	if ev["firing_count"].(int) != 2 || ev["pending_count"].(int) != 1 {
		t.Errorf("counts = %v / %v", ev["firing_count"], ev["pending_count"])
	}
	// An unscoped cluster alert is relevant, and the tool must say why it kept it.
	if !strings.Contains(fmt.Sprint(ev["matching"]), "service label") {
		t.Errorf("matching rationale missing: %v", ev["matching"])
	}
}

// TestAlertsStateFilterIsPassedThrough proves the filter reaches the backend
// rather than being silently dropped.
func TestAlertsStateFilterIsPassedThrough(t *testing.T) {
	var gotState string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"alerts":[]}}`)
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	if _, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api","state":"firing"}`)); err != nil {
		t.Fatalf("alertEvidence: %v", err)
	}
	if gotState != "firing" {
		t.Errorf("state upstream = %q, want firing", gotState)
	}
}

// TestAlertsQueryFailureFailsTheTool: alerts are primary evidence for this
// tool, so a broken Prometheus must not yield an empty "all clear".
func TestAlertsQueryFailureFailsTheTool(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "prometheus down")
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	if _, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api"}`)); err == nil {
		t.Fatal("a failing alert query must surface as an error, not an empty list")
	}
}

// TestErrorSpikesReportsLatencyAndPreviousWindow covers the two additions to
// get_error_spikes: percentiles and a comparison against the prior window.
func TestErrorSpikesReportsLatencyAndPreviousWindow(t *testing.T) {
	reg := harnessWithProm(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		startSecs := mustParseFloat(t, q2start(r))
		start := time.Unix(int64(startSecs), 0).UTC()
		stepSecs := mustParseFloat(t, r.URL.Query().Get("step"))
		step := time.Duration(stepSecs * float64(time.Second))
		if step <= 0 {
			step = time.Minute
		}
		base := 0.01
		switch {
		case strings.Contains(q, "histogram_quantile"):
			base = 0.25
		case strings.Contains(q, "http_requests_total") && !strings.Contains(q, `code=~"5.."`):
			base = 10
		}
		var sb strings.Builder
		sb.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[`)
		for i := 0; i <= 30; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `["%d","%g"]`, start.Add(time.Duration(i)*step).Unix(), base)
		}
		sb.WriteString(`]}]}}`)
		_, _ = io.WriteString(w, sb.String())
	})

	j := mustJob(t, reg, `{"service":"alerts-api","window_minutes":30}`)
	ev, err := reg.errorSpikesEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("errorSpikesEvidence: %v", err)
	}

	lat, ok := ev["latency"].(map[string]any)
	if !ok {
		t.Fatalf("latency section missing; partial_failures = %v", ev["partial_failures"])
	}
	percentiles := lat["percentiles"].(map[string]any)
	for _, want := range []string{"p50", "p95", "p99"} {
		if _, ok := percentiles[want]; !ok {
			t.Errorf("missing latency %s: %v", want, percentiles)
		}
	}

	prev, ok := ev["previous_window"].(map[string]any)
	if !ok {
		t.Fatalf("previous_window section missing; partial_failures = %v", ev["partial_failures"])
	}
	if _, ok := prev["change_ratio"]; !ok {
		t.Errorf("previous_window missing change_ratio: %v", prev)
	}
	if _, ok := prev["interpretation"]; !ok {
		t.Errorf("previous_window missing interpretation: %v", prev)
	}
	pw := prev["previous_window"].(map[string]any)
	if pw["start"] == "" || pw["end"] == "" {
		t.Errorf("comparison window not reported: %v", pw)
	}
}

// TestLatencyAbsenceIsAPartialFailureNotZeros: an uninstrumented service must
// produce an explicit gap, never a fabricated 0ms percentile.
func TestLatencyAbsenceIsAPartialFailureNotZeros(t *testing.T) {
	reg := harnessWithProm(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "histogram_quantile") {
			// No histogram instrumented: empty matrix, as real Prometheus does.
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		startSecs := mustParseFloat(t, q2start(r))
		stepSecs := mustParseFloat(t, r.URL.Query().Get("step"))
		step := time.Duration(stepSecs * float64(time.Second))
		if step <= 0 {
			step = time.Minute
		}
		var sb strings.Builder
		sb.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[`)
		for i := 0; i <= 30; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `["%d","0.01"]`, time.Unix(int64(startSecs), 0).Add(time.Duration(i)*step).Unix())
		}
		sb.WriteString(`]}]}}`)
		_, _ = io.WriteString(w, sb.String())
	})

	j := mustJob(t, reg, `{"service":"alerts-api"}`)
	ev, err := reg.errorSpikesEvidence(context.Background(), j)
	if err != nil {
		t.Fatalf("errorSpikesEvidence: %v", err)
	}
	if _, ok := ev["latency"]; ok {
		t.Error("absent histogram must not produce a latency section")
	}
	joined := fmt.Sprint(ev["partial_failures"])
	if !strings.Contains(joined, "histogram") {
		t.Errorf("the missing histogram must be named in partial_failures: %v", joined)
	}
}

// TestAuditLogRecordsOutcomeAndDuration covers design rule 5: every tool call
// is observable with what ran, how long it took, and how it ended.
func TestAuditLogRecordsOutcomeAndDuration(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Config{
		PrometheusURL:        "https://prometheus.internal",
		KubernetesURL:        "https://kubernetes.default.svc",
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	reg := NewRegistry(cfg, nil, nil, log.New(&buf, "", 0))

	// Unknown tool: logged before dispatch.
	if _, err := reg.Call(context.Background(), "nope", raw(`{}`)); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	// Known tool, missing backend: a tool_error outcome.
	if _, err := reg.Call(context.Background(), "get_pod_failures", raw(`{"service":"alerts-api"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	logs := buf.String()
	for _, want := range []string{
		"audit tool=nope outcome=unknown_tool",
		"audit tool=get_pod_failures outcome=tool_error",
		"duration_ms=",
		"args_bytes=",
		"result_bytes=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit log missing %q:\n%s", want, logs)
		}
	}
	// Argument values are not logged: they can carry operator notes.
	if strings.Contains(logs, "service=") {
		t.Errorf("audit log must not record argument values:\n%s", logs)
	}
}

// TestAlertsTruncationKeepsCountsConsistent: firing_count + pending_count
// must describe the whole matching set even when the rendered list is cut,
// and the truncation must be disclosed rather than silently dropping alerts.
func TestAlertsTruncationKeepsCountsConsistent(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"status":"success","data":{"alerts":[`)
	total := maxAlertReport + 10
	for i := 0; i < total; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		state := "pending"
		if i%2 == 0 {
			state = "firing"
		}
		fmt.Fprintf(&sb,
			`{"labels":{"alertname":"A%d","service":"alerts-api"},"state":%q,"activeAt":"2026-11-17T11:%02d:00Z","value":1}`,
			i, state, i%60)
	}
	sb.WriteString(`]}}`)

	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sb.String())
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	ev, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("alertEvidence: %v", err)
	}
	if ev["alert_count"].(int) != total {
		t.Errorf("alert_count = %v, want %d (the full matching set)", ev["alert_count"], total)
	}
	if ev["returned"].(int) != maxAlertReport {
		t.Errorf("returned = %v, want %d", ev["returned"], maxAlertReport)
	}
	if ev["truncated"] != true {
		t.Error("truncation must be flagged")
	}
	alerts := ev["alerts"].([]map[string]any)
	if len(alerts) != maxAlertReport {
		t.Fatalf("rendered %d alerts, want the cap %d", len(alerts), maxAlertReport)
	}
	// Counts are taken before truncation, so they still describe everything.
	if firing, pending := ev["firing_count"].(int), ev["pending_count"].(int); firing+pending != total {
		t.Errorf("firing %d + pending %d = %d, want %d", firing, pending, firing+pending, total)
	}
	joined := fmt.Sprint(ev["partial_failures"])
	if !strings.Contains(joined, "truncated") {
		t.Errorf("truncation must appear in partial_failures: %v", joined)
	}
}

// TestAlertsMixedScopeIsDescribedHonestly: when both labelled and unscoped
// alerts are kept, the rationale must say so rather than claiming a clean
// service-label match.
func TestAlertsMixedScopeIsDescribedHonestly(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"alerts":[
          {"labels":{"alertname":"Mine","service":"alerts-api"},"state":"firing",
           "activeAt":"2026-11-17T11:00:00Z","value":1},
          {"labels":{"alertname":"Unscoped"},"state":"pending",
           "activeAt":"2026-11-17T11:01:00Z","value":1}]}}`)
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	ev, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("alertEvidence: %v", err)
	}
	matching := fmt.Sprint(ev["matching"])
	if !strings.Contains(matching, "1 alert(s) on the service label") ||
		!strings.Contains(matching, "1 unscoped") {
		t.Errorf("mixed scope must be described precisely, got %q", matching)
	}
}

// TestAlertsServiceScopeDropsOtherServices is the scoping guarantee: an alert
// naming a different service must never appear.
func TestAlertsServiceScopeDropsOtherServices(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"alerts":[
          {"labels":{"alertname":"Theirs","service":"payments-api"},"state":"firing",
           "activeAt":"2026-11-17T11:00:00Z","value":1}]}}`)
	}))
	defer prom.Close()

	reg := registryWithProm(t, prom.URL)
	ev, err := reg.alertEvidence(context.Background(), mustJob(t, reg, `{"service":"alerts-api"}`))
	if err != nil {
		t.Fatalf("alertEvidence: %v", err)
	}
	if ev["alert_count"].(int) != 0 {
		t.Errorf("alert_count = %v, want 0", ev["alert_count"])
	}
	if len(ev["alerts"].([]map[string]any)) != 0 {
		t.Errorf("alerts = %v, want none", ev["alerts"])
	}
	// An empty match must say it proves nothing, not imply all-clear.
	if !strings.Contains(fmt.Sprint(ev["partial_failures"]), "not proof") {
		t.Errorf("empty match needs an explicit caveat: %v", ev["partial_failures"])
	}
}

// --- helpers -------------------------------------------------------------

func registryWithKube(t *testing.T, url string) *Registry {
	t.Helper()
	k, err := backend.NewKubernetes(url, "tok", nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("kubernetes: %v", err)
	}
	cfg := config.Config{
		PrometheusURL:        "https://prometheus.internal",
		KubernetesURL:        url,
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	r := NewRegistry(cfg, nil, k, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return testNow }
	return r
}

func registryWithProm(t *testing.T, url string) *Registry {
	t.Helper()
	p, err := backend.NewPrometheus(url, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	cfg := config.Config{
		PrometheusURL:        url,
		KubernetesURL:        "https://kubernetes.default.svc",
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	r := NewRegistry(cfg, p, nil, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return testNow }
	return r
}

// kubefix serves one fixed fixture for every path ending in suffix.
func kubefix(t *testing.T, suffix, fixture string) string {
	t.Helper()
	return kubefixMap(t, map[string]string{suffix: fixture})
}

// kubefixMap serves a fixture per path suffix; any other path 404s. A tool
// that needs two endpoints (history needs replicasets and events) gets both
// from one server so the test exercises the real call sequence.
func kubefixMap(t *testing.T, fixtures map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for suffix, fixture := range fixtures {
			if strings.HasSuffix(r.URL.Path, "/"+suffix) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, fixture)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// harnessWithProm builds a registry whose Prometheus is a custom handler,
// wired to a real Kubernetes mock so the other sections still work.
func harnessWithProm(t *testing.T, promHandler http.HandlerFunc) *Registry {
	t.Helper()
	prom := httptest.NewServer(promHandler)
	t.Cleanup(prom.Close)
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	t.Cleanup(kube.Close)
	return newTestHarnessWith(t, prom.URL, kube.URL)
}

func newTestHarnessWith(t *testing.T, promURL, kubeURL string) *Registry {
	t.Helper()
	p, err := backend.NewPrometheus(promURL, nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	k, err := backend.NewKubernetes(kubeURL, "tok", nil, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("kubernetes: %v", err)
	}
	cfg := config.Config{
		PrometheusURL:        promURL,
		KubernetesURL:        kubeURL,
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	r := NewRegistry(cfg, p, k, log.New(io.Discard, "", 0))
	r.now = func() time.Time { return testNow }
	return r
}

func q2start(r *http.Request) string { return r.URL.Query().Get("start") }

func mustParseFloat(t *testing.T, s string) float64 {
	t.Helper()
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return f
}

func decodeResult(t *testing.T, res Result) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Content[0].Text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return payload
}
