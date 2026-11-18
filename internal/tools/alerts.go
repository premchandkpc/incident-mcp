package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"incident-mcp/internal/backend"
)

// maxAlertReport caps how many alerts are echoed back so a fleet-wide alert
// storm cannot flood the response.
const maxAlertReport = 50

// maxAnnotationBytes bounds one rendered annotation.
const maxAnnotationBytes = 300

// alertEvidence returns the alerts Prometheus currently knows about. The
// window is accepted for interface parity with the other evidence functions
// but does not filter: /api/v1/alerts reports *active* alerts, and a stale
// alert is exactly what an operator needs to see.
func (r *Registry) alertEvidence(ctx context.Context, j job) (map[string]any, error) {
	if r.prom == nil {
		return nil, fmt.Errorf("prometheus backend is not configured")
	}
	state := j.AlertState
	alerts, err := r.prom.Alerts(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("prometheus alerts query failed: %w", err)
	}

	partial := []string{}
	service := j.Service

	matched := make([]backend.Alert, 0, len(alerts))
	for _, a := range alerts {
		// An alert either carries a service label naming this service, or no
		// service label at all (a cluster-level alert). Alerts for a different
		// service are excluded so the tool stays scoped.
		if s := a.Service(); s != "" && s != service {
			continue
		}
		matched = append(matched, a)
	}

	// Prefer alerts that explicitly name the service; if none do, fall back to
	// reporting everything that was not explicitly scoped elsewhere, and say so.
	explicit, unscoped := 0, 0
	for _, a := range matched {
		if a.Service() == service {
			explicit++
		} else {
			unscoped++
		}
	}
	var scoped string
	switch {
	case unscoped == 0:
		scoped = "matched on the service label"
	case explicit == 0:
		scoped = "no alert carried a service label for this service; showing all unscoped alerts"
	default:
		scoped = fmt.Sprintf("matched %d alert(s) on the service label and included %d unscoped alert(s) that may still be relevant", explicit, unscoped)
	}

	firing, pending := 0, 0
	items := make([]map[string]any, 0, len(matched))
	for _, a := range matched {
		switch a.State {
		case "firing":
			firing++
		case "pending":
			pending++
		}
		items = append(items, map[string]any{
			"name":      a.Name(),
			"state":     a.State,
			"active_at": a.ActiveAt.UTC().Format(time.RFC3339),
			"labels":    a.Labels,
			"summary":   bound(cleanText(a.Summary()), maxAnnotationBytes),
			"value":     roundSig(a.Value),
			"service":   a.Service(),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		// Firing first, then newest first: the alert that just started is the
		// one that explains the page you are looking at.
		if items[i]["state"] != items[j]["state"] {
			return items[i]["state"] == "firing"
		}
		return fmt.Sprint(items[i]["active_at"]) > fmt.Sprint(items[j]["active_at"])
	})

	// Counts are taken before truncation so firing_count + pending_count still
	// describe the whole matching set; `returned` reports what was rendered.
	returned := len(items)
	if returned > maxAlertReport {
		partial = append(partial, fmt.Sprintf(
			"alert list truncated to the most recent %d of %d active alerts", maxAlertReport, returned))
		items = items[:maxAlertReport]
	}
	if state != "" {
		partial = append(partial, fmt.Sprintf("filtered to alerts in state %q", state))
	}
	if len(matched) == 0 {
		partial = append(partial, "no active alerts matched; this is not proof that nothing is wrong")
	}

	return map[string]any{
		"service":          service,
		"namespace":        r.cfg.AllowedNamespace,
		"window":           j.window(),
		"state_filter":     state,
		"alert_count":      len(matched),
		"returned":         len(items),
		"truncated":        returned > maxAlertReport,
		"firing_count":     firing,
		"pending_count":    pending,
		"matching":         scoped,
		"alerts":           items,
		"partial_failures": partial,
		"notes": []string{
			"Alerts come from Prometheus /api/v1/alerts (the rules Prometheus is evaluating), not from Alertmanager; silences and notifications applied downstream are not visible here.",
			"An empty alert list does not mean the service is healthy.",
			noteRedaction,
		},
	}, nil
}

// bound truncates s to at most n bytes without splitting a UTF-8 rune.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func (r *Registry) handleAlerts(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	ev, err := r.alertEvidence(ctx, j)
	if err != nil {
		return errorResult(err)
	}
	return jsonResult(ev, false)
}
