# Production Incident Investigator MCP (Go)

A read-only, stdio MCP reference implementation using the Go standard library only. It gathers raw
evidence from Prometheus and the Kubernetes API for one service and returns it with **explicit
partial failures** instead of guessing.

```
incident-mcp/
├── cmd/incident-mcp/          main: env → backends → MCP server on stdio
└── internal/
    ├── config/                env loading + validation (no secrets in output)
    ├── validate/              DNS labels, service prefix, endpoints, time windows, page limits
    ├── backend/               read-only Prometheus + Kubernetes HTTP clients (GET only)
    ├── tools/                 tool definitions, schemas, evidence collection
    └── mcp/                   JSON-RPC 2.0 framing, resources, prompts
```

## Run

```sh
go test ./...
go run ./cmd/incident-mcp
```

Configure an MCP client to launch `go run ./cmd/incident-mcp` from this directory. Set:

| Variable | Example | Meaning |
| --- | --- | --- |
| `PROMETHEUS_URL` | `https://prometheus.internal` | Fixed Prometheus base URL (HTTPS required off-loopback) |
| `KUBERNETES_URL` | `https://kubernetes.default.svc` | Fixed Kubernetes API base URL (HTTPS required off-loopback) |
| `KUBERNETES_TOKEN` | *(scoped ServiceAccount token)* | Sent only to `KUBERNETES_URL`, never logged |
| `ALLOWED_NAMESPACE` | `production` | The only namespace the server will query |
| `ALLOWED_SERVICE_PREFIX` | `alerts-` | Services must start with this prefix |
| `MAX_WINDOW_MINUTES` | `60` | Hard cap on any requested time window (default `60`, max `10080`) |

Startup fails fast on invalid configuration: missing endpoints, a plaintext non-loopback endpoint,
credentials embedded in a URL, an invalid namespace, or a non-integer window.

Example (stdio, newline-delimited JSON-RPC):

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"demo","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"investigate_incident","arguments":{"service":"alerts-api","window_minutes":30}}}' |
  go run ./cmd/incident-mcp
```

## Protocol surface

Methods served over newline-delimited JSON-RPC 2.0 on stdio — `initialize`, `ping`, `tools/list`,
`tools/call`, `resources/list`, `resources/read`, `prompts/list`, `prompts/get`. Logs go to stderr;
stdout carries responses only.

* One line in → at most one response out, processed in order.
* Notifications (no `id`) are acknowledged by silence, including `notifications/initialized`.
* Errors use `-32700` parse, `-32600` invalid request, `-32601` unknown method, `-32602` invalid
  params/unknown tool, `-32002` unknown resource.
* Supported protocol versions: `2025-06-18`, `2025-03-26`, `2024-11-05` (the client's version is
  echoed back when supported).

## Tools

All tools take `{"service": "<name>"}`. The incident, error-spike and deploy-history tools also
accept `window_minutes`, `start` and `end` (RFC3339).

| Tool | What it does |
| --- | --- |
| `investigate_incident` | Runs the error, pod and deployment sections in one call, then merges evidence, `failed_sections` and `partial_failures` |
| `get_alerts` | Prometheus `/api/v1/alerts`: firing and pending alerts for the service, with labels, annotations, active-since times and a `state` filter |
| `get_deploy_history` | **What changed.** ReplicaSet revisions newest-first (each with the image it shipped and whether it is current) plus Kubernetes events for the service inside the window |
| `get_error_spikes` | Prometheus `http_requests_total` 5xx rate over the window: raw series, robust spike detection, peak error ratio, latency percentiles (p50/p95/p99) and the change against the previous window of equal length |
| `get_pod_failures` | Kubernetes pods for `app=<service>`: Failed/Pending phases, CrashLoopBackOff, image pull errors, OOMKilled, restart counts, failed readiness |
| `get_recent_deployments` | Current Deployment objects: desired/ready/available/updated replicas, images, revision annotation, conditions. Current state only — use `get_deploy_history` for what changed |

Notes that apply to every tool:

* Argument validation happens before any network call: service names must be lowercase DNS labels
  and must start with `ALLOWED_SERVICE_PREFIX`; windows are clamped to `MAX_WINDOW_MINUTES`
  (the response reports `clamped_to_max_window`).
* **Every list is paginated.** `limit` is sent upstream on every request (default `50`, hard cap
  `500`), so one response can never grow without bound. When more items exist the response
  carries `page.has_more: true`, `page.next_continue` and a `partial_failures` entry; pass that
  token back as `continue` to fetch the next page. `continue` is validated as a Kubernetes-style
  token (base64url character class, max 4096 chars) before it reaches an API server.
* A tool that cannot produce its primary evidence returns `isError: true`. Degraded-but-useful
  answers stay `isError: false` and list the gap under `partial_failures`.
* Detector: `value > median + 3.5*(1.4826*MAD)`; when `MAD == 0`, `value > 1.05*median`. A
  mean+3σ threshold is used for reference stats only — outliers inflate the very threshold they
  are measured against and dense bursts can hide from it.
* Latency percentiles assume `http_request_duration_seconds_bucket`. If the histogram is not
  instrumented, the `latency` key is absent and `partial_failures` says so explicitly — never a
  fabricated `0`.
* `get_deploy_history` scopes events to objects it has already proven belong to the service via
  the `app=<service>` label selector (exact ReplicaSet name, or a pod named
  `<replicaset>-<random>`). It does **not** prefix-match the bare service name, which would
  incorrectly pull in a sibling service such as `alerts-api-v2`.

Example output shape for `investigate_incident`:

```json
{
  "service": "alerts-api",
  "namespace": "production",
  "window": {"start": "2026-11-17T11:30:00Z", "end": "2026-11-17T12:00:00Z", "minutes": 30, "clamped_to_max_window": false},
  "evidence": {"error_spikes": {"...": "..."}, "pod_failures": {"...": "..."}, "recent_deployments": {"...": "..."}},
  "failed_sections": [],
  "partial_failures": [],
  "notes": ["Prometheus metric names and labels are illustrative and must match your instrumentation.", "..."]
}
```

Example page envelope returned by every list:

```json
"page": {"requested_limit": 50, "returned": 12, "has_more": true, "next_continue": "eyJwYWdlIjoyfQ=="}
```

## Resources

| URI | Contents |
| --- | --- |
| `incident://scope` | Plain-text summary of namespace, prefix, window cap, endpoints and every limitation below |
| `incident://config` | Effective configuration as JSON — the bearer token is replaced by `set (N bytes, redacted)` |
| `incident://tools` | Tool catalogue with JSON Schema input contracts |

## Prompts

| Name | Arguments | Purpose |
| --- | --- | --- |
| `investigate-service` | `service` (required), `window_minutes`, `symptom` | Ordered read-only triage plan with interpretation rules |
| `draft-postmortem-summary` | `service`, `evidence` (required), `outcome` | Conservative summary template with an "Unknowns and gaps" section |

Missing or out-of-scope prompt arguments are rejected with `-32602` before any tool runs.

## Scope and limitations

This is a working, dependency-free MVP, not a finished production platform. It retrieves raw
evidence from Prometheus and Kubernetes and returns explicit partial failures. It does not yet
integrate Loki, Tempo, OpenTelemetry SDK, production auth for remote HTTP, service-specific
config, rate limits, persistent audit storage, or automated root-cause confidence scoring. Do not
deploy as an internet-facing service.

Prometheus metric names and labels are illustrative and must match your instrumentation
(`http_requests_total{service,code}` is what the error-rate queries assume, and
`http_request_duration_seconds_bucket{service}` is what the latency percentiles assume).
Kubernetes resources are selected using `app=<service>`; adapt this to your actual deployment
labels.

`get_recent_deployments` shows current objects, not historical rollout events.
`get_deploy_history` reconstructs change history from **ReplicaSet revisions and in-window
Events**, which is the Kubernetes-native record of what was rolled out. It does not know which
CI pipeline or human triggered a rollout, and Events are ephemeral (subject to Kubernetes' event
TTL), so history older than that retention is not recoverable — use Kubernetes audit logs or CD
history for that.

`get_alerts` reads Prometheus rule state only. Alertmanager silences, notifications and
inhibition are invisible to it.

The backend HTTP client uses fixed configured endpoints and validates user-controlled DNS label
parameters. Use HTTPS and trusted internal endpoints. A Kubernetes bearer token is sent only to
the configured Kubernetes API. Never log credentials. For production use, prefer workload
identity and strict network egress policies.

## Audit log

Every `tools/call` is written to stderr as a single structured line:

```
audit tool=get_pod_failures outcome=ok duration_ms=42 args_bytes=31 result_bytes=1840
```

`outcome` is `ok`, `tool_error` (the tool ran and reported `isError`) or `unknown_tool`. Argument
*values* are never logged — they can carry operator notes — only their byte size, alongside the
result size and duration. This is in-process logging; ship it to your log pipeline for retention.
There is no persistent audit store, and there are no mutating tools to audit.

## Security

* **Kubernetes RBAC**: grant only `get`/`list` on `pods`, `deployments`, `replicasets` and
  `events` in the allowed namespace. Do not grant secrets, exec, create, update, patch, or delete.
* **Prometheus**: restrict access to it; the client issues only `GET .../api/v1/query_range` and
  `GET .../api/v1/alerts`.
* **Untrusted telemetry**: treat returned telemetry as untrusted and potentially sensitive —
  implement field-level redaction before wider use. Cluster messages are control-character
  stripped and length-bounded before they reach a response.
* **Credentials**: the token exists in memory only, is attached per-request to the configured
  Kubernetes base URL, and is described as `set (N bytes, redacted)` everywhere else. Redirects
  are never followed, so credentials cannot be forwarded to another host.
* **Input**: service names become label selectors and PromQL label values; they are validated as
  DNS labels (and prefix-checked) before use. Time windows are bounded. Page limits are clamped
  upstream. Continue tokens are character-class and length validated before being sent to an API
  server. Response lines are capped at 4 MiB and upstream error bodies at 2 KiB.

## Testing

```sh
go test ./...          # unit + end-to-end protocol tests
go test -race ./...    # race detector
go test -cover ./...   # ~90% on library packages
```

Coverage includes: configuration validation and token redaction, input/window/page/continue-token
validation, both backend clients (including "does not follow redirects", "does not leak the bearer
token", and "sends a limit on every list"), spike detection, latency and previous-window
comparison, pod failure triage, rollout history and event scoping, alert scoping and ordering,
pagination truncation reporting, audit logging, partial-failure paths, and a full stdio session
exercising every MCP method plus the error codes.

## Suggested next steps

1. Adopt official Go MCP SDK for mature transport/protocol handling.
2. Add Loki and Tempo adapters, bounded log sampling, and redaction.
3. Add service catalog, tenant isolation, and policy enforcement.
4. Add an external audit sink (today's audit log is in-process stderr) and per-principal quotas.
5. Add `build_timeline` correlating deploy history, alerts and pod events into one ordered list.
6. Add evaluation fixtures and chaos-testing integration.
