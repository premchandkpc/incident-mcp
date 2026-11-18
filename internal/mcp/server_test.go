package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"incident-mcp/internal/backend"
	"incident-mcp/internal/config"
	"incident-mcp/internal/mcp"
	"incident-mcp/internal/tools"
)

const e2eToken = "e2e-serviceaccount-token-do-not-log"

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// promHandler serves a matrix series for any query: a flat baseline with one
// injected spike when the query filters on 5xx codes.
func promHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("prometheus method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("prometheus path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		// Prometheus sends start/end as unix seconds, not RFC3339.
		startSecs, err := strconv.ParseFloat(q.Get("start"), 64)
		if err != nil {
			t.Errorf("prometheus start %q: %v", q.Get("start"), err)
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		start := time.Unix(int64(startSecs), int64(startSecs*1e9)%1e9).UTC()
		stepSecs, _ := strconv.ParseFloat(q.Get("step"), 64)
		step := time.Duration(stepSecs * float64(time.Second))
		if step <= 0 {
			step = time.Minute
		}

		errorSeries := strings.Contains(q.Get("query"), `code=~"5.."`)
		base := 10.0
		if errorSeries {
			base = 0.01
		}

		var sb strings.Builder
		sb.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"service":"alerts-api"},"values":[`)
		end := start.Add(60 * step)
		for i := 0; !start.After(end); i++ {
			v := base
			if errorSeries && i == 40 {
				v = 0.9
			}
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `["%d","%g"]`, start.Unix(), v)
			start = start.Add(step)
		}
		sb.WriteString(`]}]}}`)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sb.String())
	}
}

const e2ePodList = `{"kind":"PodList","items":[
  {"metadata":{"name":"alerts-api-a","namespace":"production","creationTimestamp":"2026-11-17T08:00:00Z","labels":{"app":"alerts-api"}},
   "spec":{"nodeName":"node-a","containers":[{"name":"app","image":"registry.internal/alerts-api:1.2.3"}]},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],
     "containerStatuses":[{"name":"app","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-11-17T08:00:05Z"}},"lastState":{}}]}},
  {"metadata":{"name":"alerts-api-b","namespace":"production","creationTimestamp":"2026-11-17T08:00:00Z","labels":{"app":"alerts-api"}},
   "spec":{"containers":[{"name":"app","image":"registry.internal/alerts-api:1.2.4"}]},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"False","reason":"ContainersNotReady"}],
     "containerStatuses":[{"name":"app","ready":false,"restartCount":7,
       "state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off restarting failed container"}},
       "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-11-17T11:00:00Z"}}}]}}]}`

const e2eDeploymentList = `{"kind":"DeploymentList","items":[
  {"metadata":{"name":"alerts-api","namespace":"production","creationTimestamp":"2026-11-10T10:00:00Z",
     "annotations":{"deployment.kubernetes.io/revision":"14"},"labels":{"app":"alerts-api"}},
   "spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"app","image":"registry.internal/alerts-api:1.2.4"}]}}},
   "status":{"replicas":3,"updatedReplicas":3,"readyReplicas":2,"availableReplicas":2,"observedGeneration":42,
     "conditions":[{"type":"Available","status":"False","reason":"MinimumReplicasUnavailable"}]}}]}`

func kubeHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("kubernetes method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+e2eToken {
			t.Errorf("Authorization = %q, want the configured bearer token", got)
		}
		if got := r.URL.Query().Get("labelSelector"); got != "app=alerts-api" {
			t.Errorf("labelSelector = %q, want app=alerts-api", got)
		}
		if !strings.Contains(r.URL.Path, "/namespaces/production/") {
			t.Errorf("path %q escapes the allowed namespace", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pods"):
			_, _ = io.WriteString(w, e2ePodList)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			_, _ = io.WriteString(w, e2eDeploymentList)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func TestServerEndToEnd(t *testing.T) {
	prom := httptest.NewServer(promHandler(t))
	defer prom.Close()
	kube := httptest.NewServer(kubeHandler(t))
	defer kube.Close()

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	cfg := config.Config{
		PrometheusURL:        prom.URL,
		KubernetesURL:        kube.URL,
		KubernetesToken:      e2eToken,
		AllowedNamespace:     "production",
		AllowedServicePrefix: "alerts-",
		MaxWindowMinutes:     60,
	}
	p, err := backend.NewPrometheus(cfg.PrometheusURL, nil, logger)
	if err != nil {
		t.Fatalf("prometheus client: %v", err)
	}
	k, err := backend.NewKubernetes(cfg.KubernetesURL, cfg.KubernetesToken, nil, logger)
	if err != nil {
		t.Fatalf("kubernetes client: %v", err)
	}
	srv := mcp.New(cfg, tools.NewRegistry(cfg, p, k, logger), logger)

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"e2e","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_pod_failures","arguments":{"service":"alerts-api"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_error_spikes","arguments":{"service":"alerts-api","window_minutes":30}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_recent_deployments","arguments":{"service":"alerts-api"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"investigate_incident","arguments":{"service":"alerts-api"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":9,"method":"resources/read","params":{"uri":"incident://scope"}}`,
		`{"jsonrpc":"2.0","id":10,"method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":11,"method":"prompts/get","params":{"name":"investigate-service","arguments":{"service":"alerts-api"}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"get_pod_failures","arguments":{"service":"payments-api"}}}`,
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"no_such_tool"}}`,
		`{"jsonrpc":"2.0","id":14,"method":"resources/read","params":{"uri":"incident://missing"}}`,
		`{"jsonrpc":"2.0","id":15,"method":"unknown/method"}`,
		`{"jsonrpc":"1.0","id":16,"method":"ping"}`,
		`not json at all`,
	}

	var in bytes.Buffer
	for _, line := range requests {
		in.WriteString(line)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), &in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	// The notification must not produce a response: 16 requests expect a
	// reply, plus the parse-error reply.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 17 {
		t.Fatalf("got %d response lines, want 17:\n%s", len(lines), out.String())
	}

	responses := make([]rpcResponse, 0, len(lines))
	byID := map[string]rpcResponse{}
	for _, line := range lines {
		var resp rpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, line)
		}
		if resp.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q", resp.JSONRPC)
		}
		responses = append(responses, resp)
		byID[string(resp.ID)] = resp
	}

	t.Run("initialize", func(t *testing.T) {
		resp := byID["1"]
		if resp.Error != nil {
			t.Fatalf("error: %+v", resp.Error)
		}
		var init struct {
			ProtocolVersion string                         `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage     `json:"capabilities"`
			ServerInfo      struct{ Name, Version string } `json:"serverInfo"`
			Instructions    string                         `json:"instructions"`
		}
		if err := json.Unmarshal(resp.Result, &init); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if init.ProtocolVersion != "2025-06-18" {
			t.Errorf("protocolVersion = %q", init.ProtocolVersion)
		}
		for _, cap := range []string{"tools", "resources", "prompts"} {
			if _, ok := init.Capabilities[cap]; !ok {
				t.Errorf("missing capability %q", cap)
			}
		}
		if init.ServerInfo.Name != mcp.ServerName || init.ServerInfo.Version == "" {
			t.Errorf("serverInfo = %+v", init.ServerInfo)
		}
		if !strings.Contains(init.Instructions, "production") {
			t.Errorf("instructions should mention the allowed namespace: %q", init.Instructions)
		}
	})

	t.Run("ping", func(t *testing.T) {
		if string(byID["2"].Result) != "{}" {
			t.Errorf("ping result = %s", byID["2"].Result)
		}
	})

	t.Run("tools/list", func(t *testing.T) {
		var list struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(byID["3"].Result, &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(list.Tools) != 4 {
			t.Fatalf("got %d tools, want 4", len(list.Tools))
		}
		for _, tool := range list.Tools {
			if tool.Description == "" || tool.InputSchema["type"] != "object" {
				t.Errorf("tool %s is malformed", tool.Name)
			}
		}
	})

	t.Run("get_pod_failures", func(t *testing.T) {
		ev := callJSON(t, byID["4"])
		if ev["pod_count"].(float64) != 2 {
			t.Errorf("pod_count = %v", ev["pod_count"])
		}
		if ev["degraded_pods"].(float64) != 1 {
			t.Errorf("degraded_pods = %v", ev["degraded_pods"])
		}
		if ev["namespace"] != "production" || ev["selector"] != "app=alerts-api" {
			t.Errorf("scope fields wrong: %v %v", ev["namespace"], ev["selector"])
		}
		failures := ev["failures"].([]any)
		if len(failures) != 1 {
			t.Fatalf("failures = %v", failures)
		}
		text := fmt.Sprint(failures[0])
		if !strings.Contains(text, "CrashLoopBackOff") || !strings.Contains(text, "OOMKilled") {
			t.Errorf("failure evidence missing container detail: %s", text)
		}
	})

	t.Run("get_error_spikes", func(t *testing.T) {
		ev := callJSON(t, byID["5"])
		spikes := ev["spikes"].([]any)
		if len(spikes) != 1 {
			t.Fatalf("got %d spikes, want 1: %v", len(spikes), spikes)
		}
		stats := ev["statistics"].(map[string]any)
		if stats["points"].(float64) < 10 {
			t.Errorf("statistics points = %v", stats["points"])
		}
		if _, ok := ev["peak_error_ratio"]; !ok {
			t.Errorf("peak_error_ratio missing: %v", keys(ev))
		}
		if len(ev["error_rate_series"].([]any)) == 0 {
			t.Error("raw evidence series missing")
		}
		partial := ev["partial_failures"].([]any)
		if len(partial) != 0 {
			t.Errorf("unexpected partial failures: %v", partial)
		}
	})

	t.Run("get_recent_deployments", func(t *testing.T) {
		ev := callJSON(t, byID["6"])
		deps := ev["deployments"].([]any)
		if len(deps) != 1 {
			t.Fatalf("deployments = %v", deps)
		}
		dep := deps[0].(map[string]any)
		if dep["revision"] != "14" {
			t.Errorf("revision = %v", dep["revision"])
		}
		if dep["rollout_complete"].(bool) {
			t.Error("deployment with 2/3 ready should not report a complete rollout")
		}
		obs := ev["observations"].([]any)
		if len(obs) != 1 {
			t.Errorf("observations = %v", obs)
		}
		notes := fmt.Sprint(ev["notes"])
		if !strings.Contains(notes, "not historical rollout events") {
			t.Errorf("notes must state the historical-rollout limitation: %s", notes)
		}
	})

	t.Run("investigate_incident", func(t *testing.T) {
		ev := callJSON(t, byID["7"])
		for _, section := range []string{"error_spikes", "pod_failures", "recent_deployments"} {
			nested, ok := ev["evidence"].(map[string]any)[section]
			if !ok || nested == nil {
				t.Errorf("evidence.%s missing or null", section)
			}
		}
		if failed := ev["failed_sections"].([]any); len(failed) != 0 {
			t.Errorf("failed_sections = %v", failed)
		}
		if partial := ev["partial_failures"].([]any); len(partial) != 0 {
			t.Errorf("partial_failures = %v", partial)
		}
		if ev["namespace"] != "production" {
			t.Errorf("namespace = %v", ev["namespace"])
		}
		if !strings.Contains(fmt.Sprint(ev["notes"]), "confidence scoring") {
			t.Error("notes should disclose the absence of confidence scoring")
		}
	})

	t.Run("resources", func(t *testing.T) {
		var list struct {
			Resources []struct{ URI, Name, MimeType string } `json:"resources"`
		}
		if err := json.Unmarshal(byID["8"].Result, &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(list.Resources) != 3 {
			t.Fatalf("resources = %+v", list.Resources)
		}

		var read struct {
			Contents []struct {
				URI      string `json:"uri"`
				MimeType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(byID["9"].Result, &read); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(read.Contents) != 1 || read.Contents[0].MimeType != "text/plain" {
			t.Fatalf("contents = %+v", read.Contents)
		}
		text := read.Contents[0].Text
		if !strings.Contains(text, "Allowed namespace") || !strings.Contains(text, "production") {
			t.Errorf("scope resource should describe the namespace:\n%s", text)
		}
		if strings.Contains(text, e2eToken) {
			t.Error("scope resource leaked the bearer token")
		}
	})

	t.Run("prompts", func(t *testing.T) {
		var list struct {
			Prompts []struct {
				Name      string `json:"name"`
				Arguments []struct {
					Name     string `json:"name"`
					Required bool   `json:"required"`
				} `json:"arguments"`
			} `json:"prompts"`
		}
		if err := json.Unmarshal(byID["10"].Result, &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(list.Prompts) != 2 {
			t.Fatalf("prompts = %+v", list.Prompts)
		}
		if list.Prompts[0].Arguments[0].Name != "service" || !list.Prompts[0].Arguments[0].Required {
			t.Errorf("first prompt should require a service argument: %+v", list.Prompts[0].Arguments)
		}

		var got struct {
			Description string `json:"description"`
			Messages    []struct {
				Role    string `json:"role"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(byID["11"].Result, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v", got.Messages)
		}
		text := got.Messages[0].Content.Text
		for _, want := range []string{"alerts-api", "get_error_spikes", "get_pod_failures", "get_recent_deployments", "partial_failures"} {
			if !strings.Contains(text, want) {
				t.Errorf("prompt text should mention %q", want)
			}
		}
	})

	t.Run("out_of_scope_service_is_a_tool_error", func(t *testing.T) {
		resp := byID["12"]
		if resp.Error != nil {
			t.Fatalf("scope violation should be a tool-level error, got protocol error %+v", resp.Error)
		}
		if !callIsError(t, resp) {
			t.Fatal("expected isError=true")
		}
	})

	t.Run("unknown_tool_is_invalid_params", func(t *testing.T) {
		if byID["13"].Error == nil || byID["13"].Error.Code != -32602 {
			t.Errorf("error = %+v, want code -32602", byID["13"].Error)
		}
	})

	t.Run("unknown_resource", func(t *testing.T) {
		if byID["14"].Error == nil || byID["14"].Error.Code != -32002 {
			t.Errorf("error = %+v, want code -32002", byID["14"].Error)
		}
	})

	t.Run("unknown_method", func(t *testing.T) {
		if byID["15"].Error == nil || byID["15"].Error.Code != -32601 {
			t.Errorf("error = %+v, want code -32601", byID["15"].Error)
		}
	})

	t.Run("bad_jsonrpc_version", func(t *testing.T) {
		if byID["16"].Error == nil || byID["16"].Error.Code != -32600 {
			t.Errorf("error = %+v, want code -32600", byID["16"].Error)
		}
	})

	t.Run("parse_error", func(t *testing.T) {
		last := responses[len(responses)-1]
		if string(last.ID) != "null" {
			t.Errorf("parse error id = %s, want null", last.ID)
		}
		if last.Error == nil || last.Error.Code != -32700 {
			t.Errorf("error = %+v, want code -32700", last.Error)
		}
	})

	t.Run("credentials_never_logged", func(t *testing.T) {
		if strings.Contains(logBuf.String(), e2eToken) {
			t.Fatalf("the bearer token was written to the log:\n%s", logBuf.String())
		}
		if !strings.Contains(logBuf.String(), "tools/call name=get_pod_failures") {
			t.Error("expected request logging to stderr")
		}
	})
}

func TestServerServeStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var in bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"ping"}\n`)
	var out bytes.Buffer
	err := mcp.New(config.Config{}, nil, log.New(io.Discard, "", 0)).Serve(ctx, &in, &out)
	if err != context.Canceled {
		t.Errorf("Serve = %v, want context.Canceled", err)
	}
}

// callJSON extracts the text content of a tools/call response and parses it as
// a JSON evidence object.
func callJSON(t *testing.T, resp rpcResponse) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", resp.Error)
	}
	var call struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &call); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if call.IsError {
		t.Fatalf("unexpected tool error: %s", call.Content[0].Text)
	}
	if len(call.Content) != 1 || call.Content[0].Type != "text" {
		t.Fatalf("content = %+v", call.Content)
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(call.Content[0].Text), &ev); err != nil {
		t.Fatalf("tool text is not JSON: %v\n%s", err, call.Content[0].Text)
	}
	return ev
}

func callIsError(t *testing.T, resp rpcResponse) bool {
	t.Helper()
	var call struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &call); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return call.IsError
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
