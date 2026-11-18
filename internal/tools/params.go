package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"incident-mcp/internal/validate"
)

// params is the raw argument object accepted by the incident tools.
type params struct {
	Service       string `json:"service"`
	WindowMinutes int    `json:"window_minutes"`
	Start         string `json:"start"`
	End           string `json:"end"`
}

// job is a validated, fully resolved investigation request.
type job struct {
	Service string
	Start   time.Time
	End     time.Time
	Minutes int
	Clamped bool
}

// decode unmarshals MCP tool arguments. Unknown fields are ignored so that
// clients sending extension arguments do not fail, but malformed JSON and
// wrong types are rejected.
func decode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// resolve validates tool arguments against the configured scope and turns
// them into an absolute time range.
func (r *Registry) resolve(raw json.RawMessage) (job, error) {
	var p params
	if err := decode(raw, &p); err != nil {
		return job{}, err
	}
	if p.Service == "" {
		return job{}, fmt.Errorf("missing required argument \"service\"")
	}
	if err := validate.Service(p.Service, r.cfg.AllowedServicePrefix); err != nil {
		return job{}, err
	}
	minutes, clamped, err := validate.Window(p.WindowMinutes, r.cfg.MaxWindowMinutes)
	if err != nil {
		return job{}, err
	}
	start, end, tClamped, err := validate.TimeRange(p.Start, p.End, minutes, r.cfg.MaxWindowMinutes, r.now())
	if err != nil {
		return job{}, err
	}
	actual := int(math.Round(end.Sub(start).Minutes()))
	if actual < 1 {
		actual = 1
	}
	return job{
		Service: p.Service,
		Start:   start,
		End:     end,
		Minutes: actual,
		Clamped: clamped || tClamped,
	}, nil
}

// window renders the resolved range for inclusion in tool output.
func (j job) window() map[string]any {
	return map[string]any{
		"start":                 j.Start.UTC().Format(time.RFC3339),
		"end":                   j.End.UTC().Format(time.RFC3339),
		"minutes":               j.Minutes,
		"clamped_to_max_window": j.Clamped,
	}
}

// stepFor picks a query step that keeps a window at or below ~120 samples
// while never going below Prometheus's practical 15s resolution.
func stepFor(start, end time.Time) time.Duration {
	secs := int(end.Sub(start).Seconds())
	if secs <= 0 {
		return 15 * time.Second
	}
	const targetPoints = 120
	step := secs / targetPoints
	if step < 15 {
		step = 15
	}
	return time.Duration(step) * time.Second
}
