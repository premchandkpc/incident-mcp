package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"incident-mcp/internal/backend"
)

// errorSpikesEvidence queries Prometheus for the service's 5xx rate and its
// total request rate. It fails only when the primary (error-rate) query
// fails; a failed secondary query is reported as a partial failure.
func (r *Registry) errorSpikesEvidence(ctx context.Context, j job) (map[string]any, error) {
	if r.prom == nil {
		return nil, fmt.Errorf("prometheus backend is not configured")
	}
	// j.Service is validated as a DNS label, so it cannot break out of the
	// PromQL label matcher below.
	errQuery := fmt.Sprintf(`sum(rate(http_requests_total{service="%s",code=~"5.."}[1m]))`, j.Service)
	totalQuery := fmt.Sprintf(`sum(rate(http_requests_total{service="%s"}[1m]))`, j.Service)
	step := stepFor(j.Start, j.End)

	errPts, err := r.prom.Range(ctx, errQuery, j.Start, j.End, step)
	if err != nil {
		return nil, fmt.Errorf("prometheus error-rate query failed: %w", err)
	}

	partial := []string{}
	stat, spikes := analyze(errPts)
	series, truncated := seriesJSON(errPts)
	if truncated {
		partial = append(partial, fmt.Sprintf("error-rate series truncated to the most recent %d samples", maxSeriesPoints))
	}
	if len(errPts) == 0 {
		partial = append(partial, "error-rate series returned no data; check that the metric names and labels match your instrumentation")
	}

	out := map[string]any{
		"service":            j.Service,
		"window":             j.window(),
		"step_seconds":       int(step.Seconds()),
		"error_rate_query":   errQuery,
		"statistics":         stat,
		"spikes":             spikes,
		"error_rate_series":  series,
		"request_rate_query": totalQuery,
		"partial_failures":   partial,
		"notes":              []string{noteMetricNames, noteRedaction},
	}

	totPts, err := r.prom.Range(ctx, totalQuery, j.Start, j.End, step)
	switch {
	case err != nil:
		partial = append(partial, fmt.Sprintf("total request-rate query failed: %v", err))
	case len(totPts) == 0:
		partial = append(partial, "total request-rate series returned no data; peak error ratio not computed")
	default:
		out["request_rate_summary"] = summarize(totPts)
		if ratio, at, ok := peakErrorRatio(errPts, totPts); ok {
			out["peak_error_ratio"] = map[string]any{
				"ratio":     roundSig(ratio),
				"timestamp": at.UTC().Format(time.RFC3339),
			}
		} else {
			partial = append(partial, "could not align error-rate and request-rate samples; peak error ratio not computed")
		}
	}
	out["partial_failures"] = partial
	return out, nil
}

// peakErrorRatio pairs the two series by exact sample timestamp, which holds
// because both queries use the same window and step.
func peakErrorRatio(errPts, totPts []backend.Point) (float64, time.Time, bool) {
	totals := make(map[int64]float64, len(totPts))
	for _, p := range totPts {
		if p.V > 0 {
			totals[p.T.UnixNano()] = p.V
		}
	}
	peak := 0.0
	var at time.Time
	found := false
	for _, p := range errPts {
		total, ok := totals[p.T.UnixNano()]
		if !ok {
			continue
		}
		ratio := p.V / total
		if ratio > peak || !found {
			peak, at, found = ratio, p.T, true
		}
	}
	return peak, at, found
}

func (r *Registry) handleErrorSpikes(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	ev, err := r.errorSpikesEvidence(ctx, j)
	if err != nil {
		return errorResult(err)
	}
	return jsonResult(ev, false)
}
