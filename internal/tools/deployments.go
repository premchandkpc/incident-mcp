package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// deploymentEvidence lists the current Deployment objects for the service and
// reports rollout status. It deliberately states that these are current
// objects: this MVP does not reconstruct historical rollout events.
func (r *Registry) deploymentEvidence(ctx context.Context, j job) (map[string]any, error) {
	if r.kube == nil {
		return nil, fmt.Errorf("kubernetes backend is not configured")
	}
	ns := r.cfg.AllowedNamespace
	selector := "app=" + j.Service
	deps, err := r.kube.ListDeployments(ctx, ns, j.Service)
	if err != nil {
		return nil, fmt.Errorf("kubernetes deployment list failed: %w", err)
	}

	now := r.now()
	items := []map[string]any{}
	observations := []string{}
	for _, d := range deps {
		desired := d.DesiredReplicas()
		rolloutComplete := d.Status.ReadyReplicas >= desired && d.Status.AvailableReplicas >= desired &&
			d.Status.UpdatedReplicas >= desired
		cond := []map[string]any{}
		for _, c := range d.Status.Conditions {
			cond = append(cond, map[string]any{
				"type":    c.Type,
				"status":  c.Status,
				"reason":  c.Reason,
				"message": cleanText(c.Message),
			})
		}
		item := map[string]any{
			"name":     d.Metadata.Name,
			"created":  d.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			"age":      humanDuration(age(d.Metadata.CreationTimestamp, now)),
			"revision": d.Metadata.Revision(),
			"images":   d.Images(),
			"replicas": map[string]any{
				"desired":   desired,
				"ready":     d.Status.ReadyReplicas,
				"available": d.Status.AvailableReplicas,
				"updated":   d.Status.UpdatedReplicas,
			},
			"rollout_complete": rolloutComplete,
			"conditions":       cond,
			"labels":           d.Metadata.Labels,
		}
		items = append(items, item)
		if !rolloutComplete {
			observations = append(observations, fmt.Sprintf(
				"deployment %q is not fully rolled out (desired %d, ready %d, available %d, updated %d)",
				d.Metadata.Name, desired, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas))
		}
	}

	partial := []string{}
	if len(deps) == 0 {
		partial = append(partial, fmt.Sprintf("no Deployments matched label selector %q in namespace %q", selector, ns))
	}

	return map[string]any{
		"service":          j.Service,
		"namespace":        ns,
		"selector":         selector,
		"deployment_count": len(deps),
		"deployments":      items,
		"observations":     observations,
		"partial_failures": partial,
		"notes":            []string{noteSelector, noteDeploymentHistory, noteRedaction},
	}, nil
}

func (r *Registry) handleDeployments(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	ev, err := r.deploymentEvidence(ctx, j)
	if err != nil {
		return errorResult(err)
	}
	return jsonResult(ev, false)
}
