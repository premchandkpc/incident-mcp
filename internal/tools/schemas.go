package tools

import (
	"fmt"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
)

// objSchema builds the JSON Schema object contract for a tool.
func objSchema(props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func serviceProperty(cfg config.Config) map[string]any {
	return map[string]any{
		"type": "string",
		"description": fmt.Sprintf(
			"Target service. Must be a lowercase DNS label (max 63 chars) and must start with the configured allowed prefix %q.",
			cfg.AllowedServicePrefix),
	}
}

func windowProperties(cfg config.Config) map[string]any {
	return map[string]any{
		"window_minutes": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     cfg.MaxWindowMinutes,
			"description": fmt.Sprintf("Look-back window in minutes. Defaults to 15 and is clamped to the server maximum of %d.", cfg.MaxWindowMinutes),
		},
		"start": map[string]any{
			"type":        "string",
			"format":      "date-time",
			"description": "Optional RFC3339 start of the window (e.g. 2026-11-17T09:00:00Z). Defaults to end minus the window.",
		},
		"end": map[string]any{
			"type":        "string",
			"format":      "date-time",
			"description": "Optional RFC3339 end of the window. Defaults to now.",
		},
	}
}

// pageProperties adds the bounded-pagination arguments shared by every tool
// that lists Kubernetes objects. The limit is always applied upstream so a
// single response can never grow without bound.
func pageProperties() map[string]any {
	return map[string]any{
		"limit": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     backend.MaxListLimit,
			"description": fmt.Sprintf("Maximum items to return per page. Defaults to %d and is capped at %d.", backend.DefaultListLimit, backend.MaxListLimit),
		},
		"continue": map[string]any{
			"type":        "string",
			"description": "Opaque continuation token from a previous call's page.next_continue. Omit it to start from the first page.",
		},
	}
}

// incidentSchema exposes window arguments plus pagination for the composite
// tool, whose pod and deployment sections both list objects.
func incidentSchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range windowProperties(cfg) {
		props[k] = v
	}
	for k, v := range pageProperties() {
		props[k] = v
	}
	return objSchema(props, []string{"service"})
}

func errorSpikesSchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range windowProperties(cfg) {
		props[k] = v
	}
	return objSchema(props, []string{"service"})
}

func podFailuresSchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range pageProperties() {
		props[k] = v
	}
	return objSchema(props, []string{"service"})
}

func deploymentsSchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range pageProperties() {
		props[k] = v
	}
	return objSchema(props, []string{"service"})
}

// historySchema takes the window (events are filtered to it) plus pagination.
func historySchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range windowProperties(cfg) {
		props[k] = v
	}
	for k, v := range pageProperties() {
		props[k] = v
	}
	return objSchema(props, []string{"service"})
}

// alertsSchema takes an optional state filter. The window is accepted for
// consistency with the other tools but does not filter active alerts.
func alertsSchema(cfg config.Config) map[string]any {
	props := map[string]any{
		"service": serviceProperty(cfg),
		"state": map[string]any{
			"type":        "string",
			"enum":        []string{"firing", "pending"},
			"description": "Only return alerts in this state. Omit to return both.",
		},
	}
	return objSchema(props, []string{"service"})
}
