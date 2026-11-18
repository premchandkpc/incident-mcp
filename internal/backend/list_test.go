package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListSendsLimitAndContinues proves the bounded-output contract at the
// transport level: every list request carries a limit, and a resume token is
// only sent when the caller supplied one.
func TestListSendsLimitAndContinues(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{
			"limit":    r.URL.Query().Get("limit"),
			"continue": r.URL.Query().Get("continue"),
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"metadata":{"continue":"next-page"},"items":[]}`)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}

	page, err := k.ListPods(context.Background(), "production", "alerts-api", ListOptions{Limit: 7})
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if got["limit"] != "7" {
		t.Errorf("limit = %q, want 7 — a list without a limit is unbounded", got["limit"])
	}
	if got["continue"] != "" {
		t.Errorf("first page sent continue=%q", got["continue"])
	}
	if page.Continue != "next-page" || !page.Truncated {
		t.Errorf("page = %+v, want the upstream continue token surfaced", page)
	}
	if page.Limit != 7 {
		t.Errorf("page.Limit = %d, want 7", page.Limit)
	}

	// Resume with the token: it must reach the API server.
	if _, err := k.ListPods(context.Background(), "production", "alerts-api",
		ListOptions{Limit: 7, Continue: "next-page"}); err != nil {
		t.Fatalf("resumed ListPods: %v", err)
	}
	if got["continue"] != "next-page" {
		t.Errorf("continue = %q, want next-page", got["continue"])
	}
}

// TestClampLimitBoundsCallerRequests: callers cannot request an unbounded
// page, and a nonsensical request falls back to the default rather than
// failing the whole investigation.
func TestClampLimitBoundsCallerRequests(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultListLimit},  // unset -> default
		{-5, DefaultListLimit}, // nonsense -> default
		{1, 1},                 // valid
		{DefaultListLimit, DefaultListLimit},
		{MaxListLimit, MaxListLimit}, // at the cap
		{MaxListLimit + 1, MaxListLimit},
		{1_000_000, MaxListLimit}, // far past the cap
	}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestPageInfoReportsPaginationWithoutLeakingNothing covers the shape every
// list tool echoes back: has_more is always present, next_continue only when
// there is actually a next page.
func TestPageInfoReportsPaginationWithoutLeakingNothing(t *testing.T) {
	full := Page[Pod]{Items: []Pod{{}, {}}, Continue: "abc", Limit: 50}
	info := full.PageInfo()
	if info["has_more"] != true || info["next_continue"] != "abc" {
		t.Errorf("info = %v", info)
	}
	if info["returned"] != 2 || info["requested_limit"] != 50 {
		t.Errorf("info = %v", info)
	}

	empty := Page[Pod]{Items: []Pod{}, Limit: 50}
	info = empty.PageInfo()
	if info["has_more"] != false {
		t.Errorf("info = %v", info)
	}
	if _, ok := info["next_continue"]; ok {
		t.Errorf("next_continue must be absent when there is no next page: %v", info)
	}
}

// TestListReplicaSetsAndEventsPaths pins the API paths: history must read
// ReplicaSets (the rollout record) and Events, never a mutation endpoint.
func TestListReplicaSetsAndEventsPaths(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET (read-only client)", r.Method)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/replicasets"):
			fmt.Fprint(w, replicaSetFixture)
		case strings.HasSuffix(r.URL.Path, "/events"):
			fmt.Fprint(w, eventFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}

	rsPage, err := k.ListReplicaSets(context.Background(), "production", "alerts-api", ListOptions{})
	if err != nil {
		t.Fatalf("ListReplicaSets: %v", err)
	}
	if got := rsPage.Items; len(got) != 1 || got[0].Revision() != "15" {
		t.Fatalf("replicasets = %+v", got)
	}
	if imgs := rsPage.Items[0].Images(); len(imgs) != 1 || imgs[0] != "registry.internal/alerts-api:1.2.5" {
		t.Errorf("images = %v", imgs)
	}
	if rsPage.Items[0].DesiredReplicas() != 3 {
		t.Errorf("desired = %d, want 3", rsPage.Items[0].DesiredReplicas())
	}

	evPage, err := k.ListEvents(context.Background(), "production", "involvedObject.kind=Pod", ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evPage.Items) != 1 {
		t.Fatalf("events = %+v", evPage.Items)
	}
	e := evPage.Items[0]
	if e.Reason != "BackOff" || e.InvolvedObject.Name != "alerts-api-abc" {
		t.Errorf("event = %+v", e)
	}
	// LastTimestamp wins over FirstTimestamp when both are set.
	if got := e.Timestamp(); got.IsZero() {
		t.Error("Timestamp should fall back to a populated field")
	}

	for _, want := range []string{
		"/apis/apps/v1/namespaces/production/replicasets",
		"/api/v1/namespaces/production/events",
	} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("path %s not requested; got %v", want, paths)
		}
	}
}

// TestListEventsRejectsInvalidNamespace: the namespace is interpolated into
// the path, so it must be validated before any request is built.
func TestListEventsRejectsInvalidNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been issued")
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.ListEvents(context.Background(), "../etc", "", ListOptions{}); err == nil {
		t.Error("invalid namespace should be rejected before any request")
	}
}

// TestPrometheusAlertsParsesAndFilters covers the alert backend: state is
// passed through, and a non-success payload is an error rather than an empty
// "all clear".
func TestPrometheusAlertsParsesAndFilters(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			t.Errorf("path = %s, want /api/v1/alerts", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		gotState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, alertFixture)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	alerts, err := p.Alerts(context.Background(), "firing")
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if gotState != "firing" {
		t.Errorf("state = %q, want firing", gotState)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Name() != "HighErrorRate" {
		t.Errorf("name = %q", alerts[0].Name())
	}
	if alerts[0].Service() != "alerts-api" {
		t.Errorf("service = %q", alerts[0].Service())
	}
	if alerts[0].Summary() == "" {
		t.Error("summary should surface an annotation")
	}
	if alerts[0].State != "firing" {
		t.Errorf("state = %q", alerts[0].State)
	}
}

// TestPrometheusAlertsErrorIsNotAnEmptyList: a broken alert endpoint must not
// read as "no alerts firing".
func TestPrometheusAlertsErrorIsNotAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	if _, err := p.Alerts(context.Background(), ""); err == nil {
		t.Fatal("expected an error for a failing alert endpoint")
	}
}

// TestAlertNameFallsBackToLabels: an alert without alertname must still be
// identifiable rather than rendering as an empty string.
func TestAlertNameFallsBackToLabels(t *testing.T) {
	a := Alert{Labels: map[string]string{"severity": "page", "team": "sre"}}
	if got := a.Name(); got == "" || !strings.Contains(got, "team=sre") {
		t.Errorf("Name() = %q, want a label-derived fallback", got)
	}
	if got := (Alert{}).Name(); got == "" {
		t.Error("a fully unlabelled alert still needs a non-empty name")
	}
	if got := (Alert{}).Summary(); got != "" {
		t.Errorf("Summary() = %q, want empty when no annotations exist", got)
	}
}

const replicaSetFixture = `{"kind":"ReplicaSetList","items":[
  {"metadata":{"name":"alerts-api-7f7d9-x1","namespace":"production",
     "creationTimestamp":"2026-11-17T11:30:00Z",
     "annotations":{"deployment.kubernetes.io/revision":"15"},
     "labels":{"app":"alerts-api"}},
   "spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"app","image":"registry.internal/alerts-api:1.2.5"}]}}},
   "status":{"replicas":3,"readyReplicas":3,"availableReplicas":3}}]}`

const eventFixture = `{"kind":"EventList","items":[
  {"metadata":{"name":"alerts-api-abc.1","namespace":"production",
     "creationTimestamp":"2026-11-17T11:50:00Z"},
   "type":"Warning","reason":"BackOff","count":4,
   "firstTimestamp":"2026-11-17T11:49:00Z","lastTimestamp":"2026-11-17T11:50:00Z",
   "involvedObject":{"kind":"Pod","name":"alerts-api-abc","uid":"u1"},
   "message":"back-off restarting failed container"}]}`

const alertFixture = `{"status":"success","data":{"alerts":[
  {"labels":{"alertname":"HighErrorRate","service":"alerts-api"},
   "annotations":{"summary":"5xx ratio above 5%"},
   "state":"firing","activeAt":"2026-11-17T11:45:00Z","value":0.07},
  {"labels":{"alertname":"PendingOne","service":"alerts-api"},
   "annotations":{},"state":"pending","activeAt":"2026-11-17T11:46:00Z","value":1}]}}`
