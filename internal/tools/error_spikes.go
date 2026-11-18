package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

	// Latency percentiles are secondary evidence: a missing histogram metric
	// must not fail a tool whose primary evidence (error rate) succeeded.
	lat, latPartial := r.latencyEvidence(ctx, j, step)
	if lat != nil {
		out["latency"] = lat
	}
	partial = append(partial, latPartial...)

	// Compare against the immediately preceding window of equal length. A
	// "high" error rate is only actionable relative to what came before.
	cmp, cmpPartial := r.previousWindowComparison(ctx, j, errPts, step)
	if cmp != nil {
		out["previous_window"] = cmp
	}
	partial = append(partial, cmpPartial...)

	out["partial_failures"] = partial
	return out, nil
}

// latencyQuantiles are the percentiles reported by latencyEvidence.
var latencyQuantiles = []struct {
	Label string
	Q     float64
}{
	{"p50", 0.50},
	{"p95", 0.95},
	{"p99", 0.99},
}

// latencyMetric is the histogram expected by latencyEvidence. It is a
// constant so the assumption is visible in one place and easy to adapt.
const latencyMetric = `http_request_duration_seconds_bucket`

// latencyEvidence queries request-duration percentiles for the window. It
// returns nil evidence (never an error) when the histogram is absent, because
// the primary evidence for get_error_spikes is the error rate, not latency.
func (r *Registry) latencyEvidence(ctx context.Context, j job, step time.Duration) (map[string]any, []string) {
	// A histogram_quantile over a service with no histogram instrumentation
	// returns an empty matrix, which is reported as a partial failure below
	// rather than as zeros that could be mistaken for "very fast".
	// One query per quantile keeps the response a plain matrix of scalars and
	// avoids depending on multi-series label juggling.
	series := make(map[string][]backend.Point, len(latencyQuantiles))
	queryFor := map[string]string{}
	var failures []string
	for _, q := range latencyQuantiles {
		query := fmt.Sprintf(
			`histogram_quantile(%g, sum by (le) (rate(%s{service="%s"}[%s])))`,
			q.Q, latencyMetric, j.Service, promWindow(step))
		pts, err := r.prom.Range(ctx, query, j.Start, j.End, step)
		if err != nil {
			failures = append(failures, fmt.Sprintf("latency %s query failed: %v", q.Label, err))
			continue
		}
		if len(pts) == 0 {
			continue
		}
		series[q.Label] = pts
		queryFor[q.Label] = query
	}

	if len(series) == 0 {
		if len(failures) == 0 {
			failures = append(failures,
				"latency percentiles returned no data; expected histogram "+latencyMetric+
					" with a le bucket label — check that latency histograms are instrumented for this service")
		}
		return nil, failures
	}

	out := map[string]any{
		"metric": latencyMetric,
		"unit":   "seconds",
		"note":   "Percentiles are computed from histogram buckets by Prometheus; they are approximations of the true distribution.",
	}
	percentiles := map[string]any{}
	for label, pts := range series {
		percentiles[label] = summarize(pts)
		if q, ok := queryFor[label]; ok {
			out[label+"_query"] = q
		}
	}
	out["percentiles"] = percentiles

	// Report which quantiles are missing so an absent p99 is never silently
	// interpreted as an absent problem.
	missing := []string{}
	for _, q := range latencyQuantiles {
		if _, ok := series[q.Label]; !ok {
			missing = append(missing, q.Label)
		}
	}
	if len(missing) > 0 {
		failures = append(failures, "no latency data for quantile(s): "+strings.Join(missing, ", "))
	}
	return out, failures
}

// previousWindowComparison re-runs the error-rate query over the window
// immediately before the requested one and reports both means plus the
// relative change. It returns nil (with a partial-failure reason) when the
// comparison cannot be made, rather than inventing a baseline.
func (r *Registry) previousWindowComparison(ctx context.Context, j job, current []backend.Point, step time.Duration) (map[string]any, []string) {
	length := j.End.Sub(j.Start)
	prevEnd := j.Start
	prevStart := j.Start.Add(-length)

	// A comparison window that starts before Prometheus's retention would
	// silently under-report; refuse rather than mislead.
	if length <= 0 {
		return nil, []string{"previous-window comparison skipped: non-positive window"}
	}
	errQuery := fmt.Sprintf(`sum(rate(http_requests_total{service="%s",code=~"5.."}[1m]))`, j.Service)
	prevPts, err := r.prom.Range(ctx, errQuery, prevStart, prevEnd, step)
	if err != nil {
		return nil, []string{fmt.Sprintf("previous-window comparison failed: %v", err)}
	}
	if len(prevPts) == 0 || len(current) == 0 {
		return nil, []string{"previous-window comparison skipped: no data in one of the windows"}
	}

	prevMean := meanOf(prevPts)
	curMean := meanOf(current)
	out := map[string]any{
		"previous_window": map[string]any{
			"start":  prevStart.UTC().Format(time.RFC3339),
			"end":    prevEnd.UTC().Format(time.RFC3339),
			"mean":   roundSig(prevMean),
			"points": len(prevPts),
		},
		"current_window": map[string]any{
			"start":  j.Start.UTC().Format(time.RFC3339),
			"end":    j.End.UTC().Format(time.RFC3339),
			"mean":   roundSig(curMean),
			"points": len(current),
		},
	}

	switch {
	case prevMean == 0 && curMean == 0:
		out["change_ratio"] = 0.0
		out["interpretation"] = "both windows are flat at zero"
	case prevMean == 0:
		out["change_ratio"] = nil
		out["interpretation"] = "previous window was zero; a ratio would be infinite, so only the absolute change is meaningful"
	default:
		out["change_ratio"] = roundSig((curMean - prevMean) / prevMean)
		if curMean > prevMean*2 && curMean-prevMean > 0.001 {
			out["interpretation"] = "error rate is more than double the previous window"
		} else if curMean < prevMean/2 {
			out["interpretation"] = "error rate is less than half the previous window"
		} else {
			out["interpretation"] = "error rate is within 2x of the previous window"
		}
	}
	return out, nil
}

// meanOf averages a series' values.
func meanOf(points []backend.Point) float64 {
	if len(points) == 0 {
		return 0
	}
	var sum float64
	for _, p := range points {
		sum += p.V
	}
	return sum / float64(len(points))
}

// promWindow renders a Prometheus range selector duration.
func promWindow(step time.Duration) string {
	secs := int(step.Seconds())
	if secs < 15 {
		secs = 15
	}
	return fmt.Sprintf("%ds", secs)
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
