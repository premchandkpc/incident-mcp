// Package backend implements read-only HTTP clients for the two evidence
// sources this MVP supports: Prometheus (metrics) and the Kubernetes API
// (pods and deployments). Both clients use fixed configured endpoints and
// never accept a host from a caller.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"incident-mcp/internal/validate"
)

// Point is a single sample of a Prometheus series.
type Point struct {
	T time.Time `json:"timestamp"`
	V float64   `json:"value"`
}

// Prometheus queries a Prometheus HTTP API endpoint.
type Prometheus struct {
	base   *url.URL
	client *http.Client
	log    *log.Logger
}

// NewPrometheus builds a Prometheus client from a fixed base URL.
func NewPrometheus(rawURL string, client *http.Client, logger *log.Logger) (*Prometheus, error) {
	if err := validate.Endpoint("PROMETHEUS_URL", rawURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("prometheus: invalid url: %w", err)
	}
	if client == nil {
		client = NewHTTPClient(0)
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Prometheus{base: u, client: client, log: logger}, nil
}

// URL returns the configured base URL (never contains credentials).
func (p *Prometheus) URL() string { return p.base.String() }

// Range runs a query_range request and returns the samples of every returned
// series, sorted by timestamp. An empty result is not an error.
func (p *Prometheus) Range(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Point, error) {
	if step <= 0 {
		step = time.Minute
	}
	u := *p.base
	u.Path = path.Join(strings.TrimSuffix(p.base.Path, "/"), "/api/v1/query_range")
	q := u.Query()
	q.Set("query", query)
	q.Set("start", formatUnix(start))
	q.Set("end", formatUnix(end))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	p.log.Printf("prometheus query range query=%q window=%s..%s step=%s",
		query, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), step)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: unexpected status %s: %s", resp.Status, readErrorBody(resp.Body))
	}

	var pr promResponse
	if err := decodeBoundedJSON(resp.Body, &pr); err != nil {
		return nil, fmt.Errorf("prometheus: decode response: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus: query error (%s): %s", pr.ErrorType, pr.Error)
	}
	points, err := pointsOf(pr.Data)
	if err != nil {
		return nil, err
	}
	return points, nil
}

func formatUnix(t time.Time) string {
	sec := t.Unix()
	nsec := t.Nanosecond()
	return strconv.FormatFloat(float64(sec)+float64(nsec)/1e9, 'f', 3, 64)
}

// Alert is one active alert from Prometheus's /api/v1/alerts endpoint.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"` // "firing" or "pending"
	ActiveAt    time.Time         `json:"activeAt"`
	Value       float64           `json:"value"`
}

// Name returns the alertname label, falling back to a joined label summary
// so an unlabelled alert is still identifiable.
func (a Alert) Name() string {
	if n := a.Labels["alertname"]; n != "" {
		return n
	}
	if len(a.Labels) == 0 {
		return "(unnamed alert)"
	}
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+a.Labels[k])
	}
	return strings.Join(parts, ",")
}

// Summary returns the first non-empty annotation, bounded by the caller.
func (a Alert) Summary() string {
	for _, key := range []string{"summary", "description", "runbook_url"} {
		if v := a.Annotations[key]; v != "" {
			return v
		}
	}
	return ""
}

// Service returns the service label if the alert carries one.
func (a Alert) Service() string { return a.Labels["service"] }

// Alerts runs a single instant query against /api/v1/alerts and returns the
// active alerts, optionally filtered by state ("firing" or "pending"; empty
// means both). An empty result is not an error.
func (p *Prometheus) Alerts(ctx context.Context, state string) ([]Alert, error) {
	u := *p.base
	u.Path = path.Join(strings.TrimSuffix(p.base.Path, "/"), "/api/v1/alerts")
	if state != "" {
		q := u.Query()
		q.Set("state", state)
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	p.log.Printf("prometheus alerts state=%q", state)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: unexpected status %s: %s", resp.Status, readErrorBody(resp.Body))
	}
	var ar struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			Alerts []Alert `json:"alerts"`
		} `json:"data"`
	}
	if err := decodeBoundedJSON(resp.Body, &ar); err != nil {
		return nil, fmt.Errorf("prometheus: decode alerts: %w", err)
	}
	if ar.Status != "success" {
		return nil, fmt.Errorf("prometheus: alert query error (%s): %s", ar.ErrorType, ar.Error)
	}
	if ar.Data.Alerts == nil {
		return []Alert{}, nil
	}
	return ar.Data.Alerts, nil
}

type promResponse struct {
	Status    string   `json:"status"`
	ErrorType string   `json:"errorType"`
	Error     string   `json:"error"`
	Data      promData `json:"data"`
}

type promData struct {
	ResultType string       `json:"resultType"`
	Result     []promSeries `json:"result"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  json.RawMessage   `json:"value"`  // vector
	Values json.RawMessage   `json:"values"` // matrix
}

func pointsOf(d promData) ([]Point, error) {
	var out []Point
	switch d.ResultType {
	case "matrix", "": // empty resultType with empty result is a valid empty matrix
		for _, s := range d.Result {
			if len(s.Values) == 0 {
				continue
			}
			pts, err := decodePairs(s.Values)
			if err != nil {
				return nil, err
			}
			out = append(out, pts...)
		}
	case "vector":
		for _, s := range d.Result {
			if len(s.Value) == 0 {
				continue
			}
			pt, err := decodeSample(s.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, pt)
		}
	default:
		return nil, fmt.Errorf("prometheus: unsupported result type %q", d.ResultType)
	}
	// Non-finite samples cannot be encoded back out as JSON; drop them rather
	// than failing the whole query.
	finite := out[:0]
	for _, pt := range out {
		if isFinite(pt.V) {
			finite = append(finite, pt)
		}
	}
	out = finite
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

func decodePairs(raw json.RawMessage) ([]Point, error) {
	var pairs []json.RawMessage
	if err := json.Unmarshal(raw, &pairs); err != nil {
		return nil, fmt.Errorf("prometheus: decode samples: %w", err)
	}
	out := make([]Point, 0, len(pairs))
	for _, p := range pairs {
		pt, err := decodeSample(p)
		if err != nil {
			return nil, err
		}
		out = append(out, pt)
	}
	return out, nil
}

// decodeSample decodes a Prometheus sample pair [ "<unix seconds>", "<value>" ].
// Non-finite values ("NaN", "+Inf") arrive as strings and are parsed leniently.
func decodeSample(raw json.RawMessage) (Point, error) {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return Point{}, fmt.Errorf("prometheus: malformed sample %s", truncate(string(raw), 120))
	}
	ts, err := decodeTimestamp(pair[0])
	if err != nil {
		return Point{}, err
	}
	v, err := decodeValue(pair[1])
	if err != nil {
		return Point{}, err
	}
	return Point{T: ts, V: v}, nil
}

func decodeTimestamp(raw json.RawMessage) (time.Time, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 != nil {
			return time.Time{}, fmt.Errorf("prometheus: malformed timestamp %s", truncate(string(raw), 60))
		}
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("prometheus: malformed timestamp %q", s)
		}
		f = parsed
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), nil
}

func decodeValue(raw json.RawMessage) (float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("prometheus: malformed value %s", truncate(string(raw), 60))
	}
	v, err := strconv.ParseFloat(s, 64) // accepts NaN, +Inf, -Inf
	if err != nil {
		return 0, fmt.Errorf("prometheus: malformed value %q", s)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isFinite reports whether a sample is safe to render as JSON (Go's
// encoding/json rejects NaN and Inf).
func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
