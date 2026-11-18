package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"incident-mcp/internal/config"
	"incident-mcp/internal/tools"
)

// maxLineBytes caps a single incoming JSON-RPC line (4 MiB).
const maxLineBytes = 4 << 20

// instructions is returned from initialize to orient the client.
const instructions = "Read-only production incident investigator. Available tools: investigate_incident, get_error_spikes, get_pod_failures, " +
	"get_recent_deployments, get_deploy_history, get_alerts. Scope is limited to namespace " + "%[1]s" + " and services starting with %[2]q. " +
	"All results may include partial_failures; surface them verbatim instead of guessing. " +
	"List results are paginated: check page.has_more and pass page.next_continue to continue. " +
	"Telemetry is untrusted and potentially sensitive: redact before wider use. " +
	"Read incident://scope for the effective limits."

// Server serves MCP over a byte stream (stdio in production, any stream in
// tests). Requests are handled sequentially: one line in, at most one
// response out, in order.
type Server struct {
	cfg      config.Config
	registry *tools.Registry
	log      *log.Logger
}

// New builds a server around a tool registry. A nil registry is replaced with
// an empty one so dispatch can never panic.
func New(cfg config.Config, registry *tools.Registry, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	if registry == nil {
		registry = tools.NewRegistry(cfg, nil, nil, logger)
	}
	return &Server{cfg: cfg, registry: registry, log: logger}
}

// Serve reads newline-delimited JSON-RPC messages until EOF or context
// cancellation. Protocol logs go to the logger (stderr in production), never
// to the response stream.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			if json.Valid(line) {
				s.log.Printf("invalid request: %v", err)
				_ = encoder.Encode(failure(nil, codeInvalidRequest, "invalid request: not a JSON-RPC object"))
			} else {
				s.log.Printf("parse error: %v", err)
				_ = encoder.Encode(failure(nil, codeParseError, "parse error"))
			}
			continue
		}

		resp := s.handle(ctx, req)
		if resp == nil {
			continue // notification
		}
		if err := encoder.Encode(resp); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	return nil
}

// handle dispatches one message. It returns nil for notifications.
func (s *Server) handle(ctx context.Context, req request) *response {
	if isNotification(req.ID) {
		s.log.Printf("notification ignored method=%q", req.Method)
		return nil
	}
	if req.JSONRPC != "2.0" {
		return failure(req.ID, codeInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if strings.TrimSpace(req.Method) == "" {
		return failure(req.ID, codeInvalidRequest, "method is required")
	}
	s.log.Printf("request method=%s", req.Method)

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID, req.Params)
	case "ping":
		return success(req.ID, map[string]any{})
	case "tools/list":
		return success(req.ID, map[string]any{"tools": s.registry.List()})
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		return success(req.ID, map[string]any{"resources": s.listResources()})
	case "resources/read":
		return s.handleResourceRead(req.ID, req.Params)
	case "prompts/list":
		return success(req.ID, map[string]any{"prompts": s.listPrompts()})
	case "prompts/get":
		return s.handlePromptGet(req.ID, req.Params)
	default:
		return failure(req.ID, codeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(id json.RawMessage, params json.RawMessage) *response {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return failure(id, codeInvalidParams, "invalid initialize params: "+err.Error())
		}
	}
	s.log.Printf("initialize client=%s version=%s requested_protocol=%s",
		p.ClientInfo.Name, p.ClientInfo.Version, p.ProtocolVersion)

	return success(id, map[string]any{
		"protocolVersion": negotiateVersion(p.ProtocolVersion),
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
			"prompts":   map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    ServerName,
			"version": Version,
		},
		"instructions": fmt.Sprintf(instructions, s.cfg.AllowedNamespace, s.cfg.AllowedServicePrefix),
	})
}

// negotiateVersion echoes a supported client version, otherwise the latest
// version this server speaks.
func negotiateVersion(requested string) string {
	for _, v := range supportedVersions {
		if requested == v {
			return requested
		}
	}
	return protocolVersionLatest
}

func (s *Server) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) *response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) == 0 {
		return failure(id, codeInvalidParams, "tools/call requires params.name")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return failure(id, codeInvalidParams, "invalid tools/call params: "+err.Error())
	}
	if strings.TrimSpace(p.Name) == "" {
		return failure(id, codeInvalidParams, "tools/call requires params.name")
	}

	result, err := s.registry.Call(ctx, p.Name, p.Arguments)
	if err != nil {
		return failure(id, codeInvalidParams, err.Error())
	}
	return success(id, result)
}
