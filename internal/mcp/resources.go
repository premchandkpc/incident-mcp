package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"incident-mcp/internal/config"
)

// resource is an MCP resource descriptor.
type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
}

func (s *Server) listResources() []resource {
	return []resource{
		{
			URI:         "incident://scope",
			Name:        "Effective scope and limitations",
			Description: "The namespace, service prefix and time-window limits this server enforces, plus the caveats that apply to every result.",
			MimeType:    "text/plain",
		},
		{
			URI:         "incident://config",
			Name:        "Redacted configuration",
			Description: "Effective configuration as JSON. The Kubernetes bearer token is never included.",
			MimeType:    "application/json",
		},
		{
			URI:         "incident://tools",
			Name:        "Tool catalogue",
			Description: "The MCP tools this server exposes with their JSON Schema input contracts.",
			MimeType:    "application/json",
		},
	}
}

// readResource renders a resource by URI. The boolean reports whether the URI
// is known.
func (s *Server) readResource(uri string) (text, mimeType string, ok bool) {
	switch uri {
	case "incident://scope":
		return s.scopeText(), "text/plain", true
	case "incident://config":
		return renderJSON(s.cfg.Redacted())
	case "incident://tools":
		return renderJSON(s.registry.List())
	default:
		return "", "", false
	}
}

func (s *Server) scopeText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Production Incident Investigator MCP (read-only) v%s\n\n", Version)
	fmt.Fprintf(&b, "Allowed namespace:        %s\n", s.cfg.AllowedNamespace)
	fmt.Fprintf(&b, "Allowed service prefix:   %s\n", s.cfg.AllowedServicePrefix)
	fmt.Fprintf(&b, "Maximum window:           %d minutes\n", s.cfg.MaxWindowMinutes)
	fmt.Fprintf(&b, "Prometheus endpoint:      %s\n", s.cfg.PrometheusURL)
	fmt.Fprintf(&b, "Kubernetes endpoint:      %s\n", s.cfg.KubernetesURL)
	fmt.Fprintf(&b, "Kubernetes credentials:   %s\n", config.TokenStatus(s.cfg.KubernetesToken))
	b.WriteString("\nTools:\n")
	for _, t := range s.registry.List() {
		fmt.Fprintf(&b, "  - %s: %s\n", t.Name, t.Description)
	}
	b.WriteString("\nLimitations:\n")
	b.WriteString("  - Evidence is raw; there is no Loki/Tempo/OpenTelemetry integration and no automated root-cause confidence scoring.\n")
	b.WriteString("  - get_recent_deployments returns current Deployment objects; use get_deploy_history for ReplicaSet revision history and in-window events.\n")
	b.WriteString("  - get_alerts reads Prometheus rule state only; Alertmanager silences and notifications are not visible.\n")
	b.WriteString("  - List results are paginated: check page.has_more and pass page.next_continue to continue.\n")
	b.WriteString("  - Results may carry explicit partial_failures; never present them as complete.\n")
	b.WriteString("  - Returned telemetry is untrusted and potentially sensitive: apply field-level redaction before wider use.\n")
	b.WriteString("  - Not an internet-facing service: keep it behind authenticated internal access and network policy.\n")
	return b.String()
}

func renderJSON(v any) (string, string, bool) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", "", false
	}
	return string(b), "application/json", true
}

func (s *Server) handleResourceRead(id json.RawMessage, params json.RawMessage) *response {
	var p struct {
		URI string `json:"uri"`
	}
	if len(params) == 0 {
		return failure(id, codeInvalidParams, "resources/read requires params.uri")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return failure(id, codeInvalidParams, "invalid resources/read params: "+err.Error())
	}
	if strings.TrimSpace(p.URI) == "" {
		return failure(id, codeInvalidParams, "resources/read requires params.uri")
	}

	text, mime, ok := s.readResource(p.URI)
	if !ok {
		return failure(id, codeResourceNotFound, "resource not found: "+p.URI)
	}
	return success(id, map[string]any{
		"contents": []map[string]any{
			{"uri": p.URI, "mimeType": mime, "text": text},
		},
	})
}
