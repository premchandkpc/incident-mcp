// Package mcp implements a minimal, read-only Model Context Protocol server
// over newline-delimited JSON-RPC 2.0 on stdio. It supports the subset needed
// by this reference implementation: initialize, ping, tools/list, tools/call,
// resources/list, resources/read, prompts/list and prompts/get.
package mcp

import "encoding/json"

// Version is the server version reported in initialize and serverInfo.
const Version = "0.1.0"

// ServerName is the MCP server name reported to clients.
const ServerName = "incident-investigator"

// Protocol versions this server can speak. The client's requested version is
// echoed back when supported.
const (
	protocolVersionLatest = "2025-06-18"
	protocolVersion202503 = "2025-03-26"
	protocolVersion202411 = "2024-11-05"
)

var supportedVersions = []string{protocolVersionLatest, protocolVersion202503, protocolVersion202411}

// JSON-RPC 2.0 and MCP error codes.
const (
	codeParseError       = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternalError    = -32603
	codeResourceNotFound = -32002 // MCP: resource not found
)

// request is an incoming JSON-RPC message. ID is absent for notifications.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is an outgoing JSON-RPC message.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// isNotification reports whether a message expects no response: JSON-RPC
// notifications carry no id (or an explicit null id).
func isNotification(id json.RawMessage) bool {
	trimmed := string(id)
	return trimmed == "" || trimmed == "null"
}

func failure(id json.RawMessage, code int, message string) *response {
	if isNotification(id) {
		id = json.RawMessage("null")
	}
	return &response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	}
}

func success(id json.RawMessage, result any) *response {
	raw, err := json.Marshal(result)
	if err != nil {
		return failure(id, codeInternalError, "failed to encode result: "+err.Error())
	}
	return &response{JSONRPC: "2.0", ID: id, Result: raw}
}
