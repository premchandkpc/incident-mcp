package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// incidentEvidence collects all three evidence sections. A section that fails
// entirely is recorded under failed_sections; section-level partial failures
// are hoisted to the top level so nothing degrades silently.
func (r *Registry) incidentEvidence(ctx context.Context, j job) (map[string]any, bool) {
	sections := []struct {
		key string
		run func(context.Context, job) (map[string]any, error)
	}{
		{"error_spikes", r.errorSpikesEvidence},
		{"pod_failures", r.podEvidence},
		{"recent_deployments", r.deploymentEvidence},
	}

	evidence := map[string]any{}
	partial := []string{}
	failed := []string{}

	for _, s := range sections {
		ev, err := s.run(ctx, j)
		if err != nil {
			failed = append(failed, s.key)
			partial = append(partial, fmt.Sprintf("%s: %v", s.key, err))
			evidence[s.key] = nil
			r.log.Printf("incident section failed section=%s service=%s err=%v", s.key, j.Service, err)
			continue
		}
		evidence[s.key] = ev
		partial = append(partial, hoistPartial(s.key, ev)...)
	}

	out := map[string]any{
		"service":          j.Service,
		"namespace":        r.cfg.AllowedNamespace,
		"window":           j.window(),
		"evidence":         evidence,
		"failed_sections":  failed,
		"partial_failures": partial,
		"notes": []string{
			noteMetricNames,
			noteSelector,
			noteDeploymentHistory,
			noteNoScoring,
			noteRedaction,
		},
	}
	return out, len(failed) == len(sections)
}

// hoistPartial prefixes a section's own partial failures with the section key.
func hoistPartial(section string, ev map[string]any) []string {
	raw, ok := ev["partial_failures"].([]string)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		out = append(out, section+": "+p)
	}
	return out
}

func (r *Registry) handleIncident(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	out, allFailed := r.incidentEvidence(ctx, j)
	return jsonResult(out, allFailed)
}
