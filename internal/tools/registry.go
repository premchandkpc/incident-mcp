// Package tools defines the MCP tools this server exposes, their JSON Schema
// input definitions, and the evidence-collection handlers behind them. The
// handlers are read-only: they query Prometheus and the Kubernetes API and
// report explicit partial failures instead of pretending a degraded answer is
// complete.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
)

// callTimeout bounds a single tools/call, including its backend requests. It
// is sized for the worst case: every section making two upstream requests at
// the backend's own 15s per-request timeout.
const callTimeout = 90 * time.Second

// Shared caveats echoed in tool output so consumers never mistake this MVP
// for a finished analysis platform.
const (
	noteMetricNames       = "Prometheus metric names and labels are illustrative and must match your instrumentation."
	noteSelector          = "Kubernetes resources are selected using app=<service>; adapt this to your actual deployment labels."
	noteDeploymentHistory = "Deployment results show current objects, not historical rollout events. Use Kubernetes audit/events or CD history for true deployment history."
	noteRedaction         = "Returned telemetry is untrusted and potentially sensitive: apply field-level redaction before wider use."
	noteNoScoring         = "No automated root-cause confidence scoring is performed; correlation is left to the operator."
)

// Content is a single MCP content block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result is the payload of an MCP tools/call result.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Tool is an MCP tool definition together with its handler.
type Tool struct {
	Name        string                                        `json:"name"`
	Description string                                        `json:"description"`
	InputSchema map[string]any                                `json:"inputSchema"`
	Handler     func(context.Context, json.RawMessage) Result `json:"-"`
}

// Registry holds the tool, resource and prompt surface of the server.
type Registry struct {
	cfg  config.Config
	prom *backend.Prometheus
	kube *backend.Kubernetes
	log  *log.Logger
	now  func() time.Time

	tools  []Tool
	byName map[string]Tool
}

// NewRegistry builds the tool registry around the configured backends.
func NewRegistry(cfg config.Config, prom *backend.Prometheus, kube *backend.Kubernetes, logger *log.Logger) *Registry {
	if logger == nil {
		logger = log.Default()
	}
	r := &Registry{
		cfg:    cfg,
		prom:   prom,
		kube:   kube,
		log:    logger,
		now:    func() time.Time { return time.Now() },
		byName: map[string]Tool{},
	}
	r.tools = []Tool{
		{
			Name:        "investigate_incident",
			Description: "Run the full read-only incident triage for one service: collect Prometheus error-rate spikes, Kubernetes pod failures, and current Deployment state, then return the evidence with explicit partial failures. Scope is limited to namespace " + cfg.AllowedNamespace + " and services starting with \"" + cfg.AllowedServicePrefix + "\".",
			InputSchema: incidentSchema(cfg),
			Handler:     r.handleIncident,
		},
		{
			Name:        "get_error_spikes",
			Description: "Query Prometheus for the 5xx error rate of one service over a bounded time window and report statistically significant spikes (robust median/MAD detector), plus the peak error ratio against total request rate.",
			InputSchema: errorSpikesSchema(cfg),
			Handler:     r.handleErrorSpikes,
		},
		{
			Name:        "get_pod_failures",
			Description: "List Kubernetes pods for one service in the allowed namespace and report failure evidence: Failed/Pending phases, CrashLoopBackOff and image pull errors, restart counts, non-zero terminations and failed readiness.",
			InputSchema: podFailuresSchema(cfg),
			Handler:     r.handlePodFailures,
		},
		{
			Name:        "get_recent_deployments",
			Description: "List the current Kubernetes Deployment objects for one service in the allowed namespace, with replica rollout status, container images and revision annotations. These are current objects, not historical rollout events.",
			InputSchema: deploymentsSchema(cfg),
			Handler:     r.handleDeployments,
		},
	}
	for _, t := range r.tools {
		r.byName[t.Name] = t
	}
	return r
}

// List returns the tool definitions in display order.
func (r *Registry) List() []Tool { return r.tools }

// Call executes a tool. A non-nil error means the tool name is unknown, which
// the MCP layer reports as an invalid-params error.
func (r *Registry) Call(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	t, ok := r.byName[name]
	if !ok {
		return Result{}, fmt.Errorf("unknown tool %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	r.log.Printf("tools/call name=%s bytes=%d", name, len(args))
	return t.Handler(ctx, args), nil
}

// jsonResult renders v as pretty JSON in a single text content block.
func jsonResult(v any, isError bool) Result {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errorResultf("failed to encode evidence: %v", err)
	}
	return Result{Content: []Content{{Type: "text", Text: string(b)}}, IsError: isError}
}

// errorResult reports a caller-correctable failure as an MCP tool error.
func errorResult(err error) Result { return errorResultf("%s", err.Error()) }

func errorResultf(format string, args ...any) Result {
	return Result{
		Content: []Content{{Type: "text", Text: "Error: " + fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}
