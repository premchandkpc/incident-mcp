package tools

import (
	"fmt"

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

func incidentSchema(cfg config.Config) map[string]any {
	props := map[string]any{"service": serviceProperty(cfg)}
	for k, v := range windowProperties(cfg) {
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
	return objSchema(map[string]any{"service": serviceProperty(cfg)}, []string{"service"})
}

func deploymentsSchema(cfg config.Config) map[string]any {
	return objSchema(map[string]any{"service": serviceProperty(cfg)}, []string{"service"})
}
