package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPrometheusRange checks parsing of a matrix response end to end.
func TestPrometheusRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		startSecs, err := strconv.ParseFloat(q.Get("start"), 64)
		if err != nil {
			t.Errorf("start %q: %v", q.Get("start"), err)
		}
		start := time.Unix(int64(startSecs), int64(startSecs*1e9)%1e9).UTC()
		stepSecs := 30.0
		if s := q.Get("step"); s != "" {
			fmt.Sscanf(s, "%g", &stepSecs)
		}
		step := time.Duration(stepSecs * float64(time.Second))

		isErrorSeries := strings.Contains(q.Get("query"), `code=~"5.."`)
		base := 10.0
		if isErrorSeries {
			base = 0.01
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{`)
		fmt.Fprint(w, `"metric":{"service":"alerts-api"},"values":[`)
		first := true
		for i := 0; i < 10; i++ {
			ts := start.Add(time.Duration(i) * step)
			v := base
			if isErrorSeries && i == 6 {
				v = 0.9
			}
			if !first {
				fmt.Fprint(w, ",")
			}
			first = false
			fmt.Fprintf(w, `["%d","%v"]`, ts.Unix(), v)
		}
		fmt.Fprint(w, `]}]}}`)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	pts, err := p.Range(context.Background(), `sum(rate(http_requests_total{service="alerts-api",code=~"5.."}[1m]))`,
		now.Add(-5*time.Minute), now, 30*time.Second)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(pts) != 10 {
		t.Fatalf("got %d points, want 10", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].T.Before(pts[i-1].T) {
			t.Fatalf("points not sorted by time at index %d", i)
		}
	}
	if pts[6].V != 0.9 {
		t.Errorf("spike point = %v, want 0.9", pts[6].V)
	}
	if pts[0].V != 0.01 {
		t.Errorf("baseline point = %v, want 0.01", pts[0].V)
	}
}

func TestPrometheusRangeSuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error: unexpected end of input"}`)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	_, err = p.Range(context.Background(), "up", time.Now().Add(-time.Minute), time.Now(), time.Minute)
	if err == nil {
		t.Fatal("expected error for non-2xx status")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("error %q should include the upstream message", err)
	}
}

func TestPrometheusDropsNonFiniteSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{},"values":[["1000","1.5"],["1060","NaN"],["1120","2.5"]]}]}}`)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	pts, err := p.Range(context.Background(), "up", time.Unix(1000, 0), time.Unix(1120, 0), time.Minute)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2 (NaN dropped)", len(pts))
	}
	if pts[0].V != 1.5 || pts[1].V != 2.5 {
		t.Errorf("values = %v", pts)
	}
}

func TestPrometheusEmptyResultIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	pts, err := p.Range(context.Background(), "up", time.Now().Add(-time.Minute), time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("got %d points, want 0", len(pts))
	}
}

func TestPrometheusDoesNotFollowRedirects(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer target.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusFound)
	}))
	defer front.Close()

	p, err := NewPrometheus(front.URL, nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	_, err = p.Range(context.Background(), "up", time.Now().Add(-time.Minute), time.Now(), time.Minute)
	if err == nil {
		t.Fatal("expected an error for a redirect response")
	}
	if redirected {
		t.Error("client followed a redirect to another host")
	}
}

func TestNewPrometheusRejectsPlaintextRemoteEndpoint(t *testing.T) {
	if _, err := NewPrometheus("http://prometheus.internal", nil, nil); err == nil {
		t.Fatal("expected error for plaintext non-loopback endpoint")
	}
	if _, err := NewPrometheus("https://prometheus.internal", nil, nil); err != nil {
		t.Fatalf("https endpoint should be accepted: %v", err)
	}
}

func TestPrometheusBasePathPreserved(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL+"/prometheus", nil, nil)
	if err != nil {
		t.Fatalf("NewPrometheus: %v", err)
	}
	if _, err := p.Range(context.Background(), "up", time.Now().Add(-time.Minute), time.Now(), time.Minute); err != nil {
		t.Fatalf("Range: %v", err)
	}
	if gotPath != "/prometheus/api/v1/query_range" {
		t.Errorf("path = %q, want /prometheus/api/v1/query_range", gotPath)
	}
}
