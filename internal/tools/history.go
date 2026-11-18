package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"incident-mcp/internal/backend"
)

// maxEventReport caps how many events are echoed back so a hot namespace
// cannot flood the response.
const maxEventReport = 50

// maxRolloutEntries caps how many ReplicaSet revisions are reported.
const maxRolloutEntries = 20

// historyEvidence reconstructs what changed for the service: the ReplicaSet
// revisions that record each rollout (with the image that revision shipped)
// and the Kubernetes events observed inside the requested window.
//
// ReplicaSets are the durable record of a rollout: every update creates one
// carrying deployment.kubernetes.io/revision and the pod template that was
// rolled out. Events supply the surrounding narrative (scaling, image pulls,
// OOM kills, readiness failures).
func (r *Registry) historyEvidence(ctx context.Context, j job) (map[string]any, error) {
	if r.kube == nil {
		return nil, fmt.Errorf("kubernetes backend is not configured")
	}
	ns := r.cfg.AllowedNamespace
	selector := "app=" + j.Service
	partial := []string{}

	page, err := r.kube.ListReplicaSets(ctx, ns, j.Service, backend.ListOptions{Limit: j.Limit, Continue: j.Continue})
	if err != nil {
		return nil, fmt.Errorf("kubernetes replicasets list failed: %w", err)
	}
	if page.Truncated {
		partial = append(partial, fmt.Sprintf(
			"replicasets truncated at the page limit of %d; re-run with the returned next_continue token to fetch the next page",
			page.Limit))
	}

	now := r.now()
	rollouts := rolloutHistory(page.Items, now)
	if len(rollouts) == 0 {
		partial = append(partial, fmt.Sprintf(
			"no ReplicaSets matched label selector %q in namespace %q; rollout history is unavailable (was the service deployed under a different label?)",
			selector, ns))
	}

	// Events are matched against object names we have already proven belong to
	// this service (via the app=<service> label selector), rather than by
	// prefixing the service name. Prefix matching is wrong here: pods are
	// named "<replicaset>-<random>", so a sibling service such as
	// "alerts-api-v2" produces pod names that also start with "alerts-api-".
	owned := make(map[string]bool, len(page.Items))
	for _, rs := range page.Items {
		owned[rs.Metadata.Name] = true
	}

	events, eventPartial := r.windowEvents(ctx, ns, j, owned)
	partial = append(partial, eventPartial...)
	return map[string]any{
		"service":          j.Service,
		"namespace":        ns,
		"selector":         selector,
		"window":           j.window(),
		"rollout_history":  rollouts,
		"rollout_count":    len(rollouts),
		"events":           events,
		"event_count":      len(events),
		"page":             page.PageInfo(),
		"partial_failures": partial,
		"notes": []string{
			noteSelector,
			noteRedaction,
			"Rollout history is derived from ReplicaSet revisions and images; it records what was rolled out, not who rolled it out or from which pipeline.",
			"Events are filtered to the requested window and to objects whose name belongs to this service; events already expired from Kubernetes' TTL are not recoverable.",
		},
	}, nil
}

// rolloutEntry is one historical revision of a service.
type rolloutEntry struct {
	Revision int      `json:"revision"`
	Name     string   `json:"replicaset"`
	Created  string   `json:"created"`
	Age      string   `json:"age"`
	Images   []string `json:"images"`
	Replicas struct {
		Desired int `json:"desired"`
		Ready   int `json:"ready"`
	} `json:"replicas"`
	Current      bool `json:"current"`
	ScaledToZero bool `json:"scaled_to_zero"`
}

// rolloutHistory converts ReplicaSets into revisions ordered newest first.
// A ReplicaSet with no parseable revision is kept but sorts last, so a
// cluster that omits the annotation still yields its history rather than an
// empty answer.
func rolloutHistory(items []backend.ReplicaSet, now time.Time) []rolloutEntry {
	out := make([]rolloutEntry, 0, len(items))
	maxRev := -1
	for _, rs := range items {
		rev := 0
		if raw := rs.Revision(); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				rev = n
			}
		}
		if rev > maxRev {
			maxRev = rev
		}
		desired := rs.DesiredReplicas()
		e := rolloutEntry{
			Revision:     rev,
			Name:         rs.Metadata.Name,
			Created:      rs.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			Age:          humanDuration(age(rs.Metadata.CreationTimestamp, now)),
			Images:       rs.Images(),
			ScaledToZero: desired == 0,
		}
		e.Replicas.Desired = desired
		e.Replicas.Ready = rs.Status.ReadyReplicas
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Revision != out[j].Revision {
			return out[i].Revision > out[j].Revision
		}
		// Equal or unknown revisions: fall back to creation time, newest first.
		return out[i].Created > out[j].Created
	})
	if len(out) > maxRolloutEntries {
		out = out[:maxRolloutEntries]
	}
	// maxRev is the highest revision seen across all items (before truncation).
	// When no cluster carries the revision annotation every entry is 0, so fall
	// back to "newest by creation time" rather than marking nothing current —
	// but only the newest entry, never all of them.
	newestIsCurrent := maxRev <= 0
	for i := range out {
		out[i].Current = (newestIsCurrent && i == 0) || (!newestIsCurrent && out[i].Revision == maxRev)
	}
	return out
}

// windowEvents gathers Kubernetes events inside the job's window that belong
// to objects this service owns. Two fieldSelector queries are issued
// (ReplicaSet and Pod); both are bounded by the page limit and then filtered
// client-side, because Kubernetes field selectors only support exact matches
// on the involved-object name while a service owns many objects.
//
// owned must contain the object names already proven to belong to the service
// by label selector. A pod's name is "<replicaset>-<random>", so ownership is
// decided by exact match against a ReplicaSet name or by prefixing one of
// them -- never by prefixing the bare service name, which would also match a
// sibling service like "alerts-api-v2".
func (r *Registry) windowEvents(ctx context.Context, ns string, j job, owned map[string]bool) ([]map[string]any, []string) {
	partial := []string{}
	var all []backend.Event
	seen := map[string]bool{}
	add := func(e backend.Event) {
		// Two fieldSelector queries can overlap (a proxy, a mislabeled API
		// server, or an event re-reported under another kind). Key on the
		// event's identity so the same observation is never shown twice.
		key := e.Metadata.UID
		if key == "" {
			key = e.Metadata.Namespace + "/" + e.Metadata.Name + "/" + e.Reason
		}
		if seen[key] {
			return
		}
		seen[key] = true
		all = append(all, e)
	}

	for _, kind := range []string{"ReplicaSet", "Pod"} {
		page, err := r.kube.ListEvents(ctx, ns,
			"involvedObject.kind="+kind,
			backend.ListOptions{Limit: j.Limit})
		if err != nil {
			// Events are supporting evidence: a failure degrades the answer
			// instead of failing the whole tool.
			partial = append(partial, fmt.Sprintf("event query for kind=%s failed: %v", kind, err))
			continue
		}
		if page.Truncated {
			partial = append(partial, fmt.Sprintf(
				"events for kind=%s truncated at the page limit of %d; the event list may be incomplete",
				kind, page.Limit))
		}
		for _, e := range page.Items {
			if !ownsObject(owned, e.InvolvedObject.Name) {
				continue
			}
			ts := e.Timestamp()
			if ts.Before(j.Start) || ts.After(j.End) {
				continue
			}
			add(e)
		}
	}

	sort.SliceStable(all, func(i, j int) bool { return all[i].Timestamp().After(all[j].Timestamp()) })
	if len(all) > maxEventReport {
		partial = append(partial, fmt.Sprintf(
			"event list truncated to the most recent %d of %d events in the window", maxEventReport, len(all)))
		all = all[:maxEventReport]
	}

	out := make([]map[string]any, 0, len(all))
	for _, e := range all {
		out = append(out, map[string]any{
			"time":    e.Timestamp().UTC().Format(time.RFC3339),
			"type":    e.Type,
			"reason":  e.Reason,
			"object":  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			"count":   e.Count,
			"message": cleanText(e.Message),
		})
	}
	return out, partial
}

// ownsObject reports whether an involved-object name belongs to the service,
// given the ReplicaSet names proven to be its own.
//
// A ReplicaSet is owned by exact name. A pod is owned when its name starts
// with an owned ReplicaSet name followed by "-", because Kubernetes names
// pods "<replicaset>-<random suffix>". Matching on the bare service name
// would be wrong: a sibling service "alerts-api-v2" owns pods that also
// begin with "alerts-api-".
func ownsObject(owned map[string]bool, name string) bool {
	if owned[name] {
		return true
	}
	for rs := range owned {
		if strings.HasPrefix(name, rs+"-") {
			return true
		}
	}
	return false
}

func (r *Registry) handleDeployHistory(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	ev, err := r.historyEvidence(ctx, j)
	if err != nil {
		return errorResult(err)
	}
	return jsonResult(ev, false)
}
