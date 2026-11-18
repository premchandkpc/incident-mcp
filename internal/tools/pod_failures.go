package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"incident-mcp/internal/backend"
)

// pendingGrace is how long a pod may sit in Pending before it is reported.
const pendingGrace = 2 * time.Minute

// restartThreshold is the restart count at which a container is reported even
// when its current state looks healthy.
const restartThreshold = 3

// podEvidence lists pods for the service and reports per-pod failure
// findings. An API error fails the section; an empty result is reported as a
// partial failure instead.
func (r *Registry) podEvidence(ctx context.Context, j job) (map[string]any, error) {
	if r.kube == nil {
		return nil, fmt.Errorf("kubernetes backend is not configured")
	}
	ns := r.cfg.AllowedNamespace
	selector := "app=" + j.Service
	pods, err := r.kube.ListPods(ctx, ns, j.Service)
	if err != nil {
		return nil, fmt.Errorf("kubernetes pod list failed: %w", err)
	}

	now := r.now()
	failures := []map[string]any{}
	summaries := []map[string]any{}
	healthy := 0

	for _, p := range pods {
		findings := podFindings(p, now)
		summaries = append(summaries, map[string]any{
			"name":     p.Metadata.Name,
			"phase":    p.Status.Phase,
			"ready":    conditionTrue(p, "Ready"),
			"restarts": restarts(p),
			"node":     firstNonEmpty(p.Status.NodeName, p.Spec.NodeName),
			"age":      humanDuration(age(p.Metadata.CreationTimestamp, now)),
			"created":  p.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			"images":   p.Images(),
			"findings": findings,
		})
		if len(findings) > 0 {
			failures = append(failures, map[string]any{
				"pod":      p.Metadata.Name,
				"phase":    p.Status.Phase,
				"age":      humanDuration(age(p.Metadata.CreationTimestamp, now)),
				"findings": findings,
			})
		} else {
			healthy++
		}
	}

	partial := []string{}
	if len(pods) == 0 {
		partial = append(partial, fmt.Sprintf("no pods matched label selector %q in namespace %q", selector, ns))
	}

	return map[string]any{
		"service":          j.Service,
		"namespace":        ns,
		"selector":         selector,
		"pod_count":        len(pods),
		"healthy_pods":     healthy,
		"degraded_pods":    len(pods) - healthy,
		"failures":         failures,
		"pods":             summaries,
		"partial_failures": partial,
		"notes":            []string{noteSelector, noteRedaction},
	}, nil
}

// podFindings converts raw pod status into bounded, human-readable failure
// evidence. Status messages come from the cluster and are truncated so they
// cannot flood the response.
func podFindings(p backend.Pod, now time.Time) []string {
	findings := []string{}
	add := func(f string) {
		if f != "" && len(findings) < 8 {
			for _, existing := range findings {
				if existing == f {
					return
				}
			}
			findings = append(findings, f)
		}
	}

	created := p.Metadata.CreationTimestamp
	ageStr := humanDuration(age(created, now))

	switch p.Status.Phase {
	case "Failed":
		add("pod phase Failed" + messageSuffix(p.Status.Message))
	case "Pending":
		if created.IsZero() || now.Sub(created) > pendingGrace {
			add(fmt.Sprintf("pod pending for %s", ageStr))
		}
	case "Unknown":
		add("pod phase Unknown" + messageSuffix(p.Status.Message))
	}

	all := make([]backend.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	all = append(all, p.Status.InitContainerStatuses...)
	all = append(all, p.Status.ContainerStatuses...)

	for _, cs := range all {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			add(fmt.Sprintf("container %s waiting: %s%s", cs.Name, w.Reason, messageSuffix(w.Message)))
		}
		if t := cs.State.Terminated; t != nil && terminatedUnhealthy(t.Reason, t.ExitCode) {
			add(fmt.Sprintf("container %s terminated: %s (exit %d)", cs.Name, reasonOrUnknown(t.Reason), t.ExitCode))
		}
		if t := cs.LastState.Terminated; t != nil && terminatedUnhealthy(t.Reason, t.ExitCode) {
			add(fmt.Sprintf("container %s last terminated: %s (exit %d)", cs.Name, reasonOrUnknown(t.Reason), t.ExitCode))
		}
		if cs.RestartCount >= restartThreshold {
			add(fmt.Sprintf("container %s restarted %d times", cs.Name, cs.RestartCount))
		}
	}

	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status != "True" {
			if c.Reason != "" {
				add("readiness condition " + c.Status + ": " + c.Reason)
			} else {
				add("readiness condition is " + c.Status)
			}
		}
	}
	return findings
}

func terminatedUnhealthy(reason string, exitCode int) bool {
	if reason == "Completed" {
		return false
	}
	return exitCode != 0 || reason != ""
}

func reasonOrUnknown(reason string) string {
	if reason == "" {
		return "unknown reason"
	}
	return reason
}

func messageSuffix(msg string) string {
	msg = cleanText(msg)
	if msg == "" {
		return ""
	}
	return ": " + msg
}

func cleanText(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	const max = 200
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

func conditionTrue(p backend.Pod, condType string) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == condType {
			return c.Status == "True"
		}
	}
	return false
}

func restarts(p backend.Pod) int {
	total := 0
	for _, cs := range p.Status.ContainerStatuses {
		total += cs.RestartCount
	}
	for _, cs := range p.Status.InitContainerStatuses {
		total += cs.RestartCount
	}
	return total
}

func age(created, now time.Time) time.Duration {
	if created.IsZero() {
		return 0
	}
	d := now.Sub(created)
	if d < 0 {
		return 0
	}
	return d
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *Registry) handlePodFailures(ctx context.Context, raw json.RawMessage) Result {
	j, err := r.resolve(raw)
	if err != nil {
		return errorResult(err)
	}
	ev, err := r.podEvidence(ctx, j)
	if err != nil {
		return errorResult(err)
	}
	return jsonResult(ev, false)
}
