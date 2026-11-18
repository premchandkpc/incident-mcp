package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "eyJhbGciOi.super-secret-serviceaccount-token"

const podListFixture = `{
  "kind": "PodList",
  "items": [
    {
      "metadata": {"name": "alerts-api-6f7d9-x1", "namespace": "production", "creationTimestamp": "2026-11-17T08:00:00Z", "labels": {"app": "alerts-api"}},
      "spec": {"nodeName": "node-a", "containers": [{"name": "app", "image": "registry.internal/alerts-api:1.2.3"}]},
      "status": {
        "phase": "Running",
        "conditions": [{"type": "Ready", "status": "True"}],
        "containerStatuses": [{"name": "app", "ready": true, "restartCount": 0, "state": {"running": {"startedAt": "2026-11-17T08:00:05Z"}}, "lastState": {}}]
      }
    },
    {
      "metadata": {"name": "alerts-api-6f7d9-y2", "namespace": "production", "creationTimestamp": "2026-11-17T08:00:00Z", "labels": {"app": "alerts-api"}},
      "spec": {"containers": [{"name": "app", "image": "registry.internal/alerts-api:1.2.4"}]},
      "status": {
        "phase": "Running",
        "conditions": [{"type": "Ready", "status": "False", "reason": "ContainersNotReady"}],
        "containerStatuses": [{
          "name": "app", "ready": false, "restartCount": 7,
          "state": {"waiting": {"reason": "CrashLoopBackOff", "message": "back-off 40s restarting failed container"}},
          "lastState": {"terminated": {"reason": "Error", "exitCode": 1, "finishedAt": "2026-11-17T09:00:00Z"}}
        }]
      }
    }
  ]
}`

const deploymentListFixture = `{
  "kind": "DeploymentList",
  "items": [
    {
      "metadata": {
        "name": "alerts-api",
        "namespace": "production",
        "creationTimestamp": "2026-11-10T10:00:00Z",
        "annotations": {"deployment.kubernetes.io/revision": "14"},
        "labels": {"app": "alerts-api"}
      },
      "spec": {
        "replicas": 3,
        "selector": {"matchLabels": {"app": "alerts-api"}},
        "template": {"spec": {"containers": [{"name": "app", "image": "registry.internal/alerts-api:1.2.4"}]}}
      },
      "status": {
        "replicas": 3, "updatedReplicas": 3, "readyReplicas": 3, "availableReplicas": 3, "observedGeneration": 42,
        "conditions": [{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable", "message": "Deployment has minimum availability."}]
      }
    }
  ]
}`

func TestListPodsSendsScopedBearerToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET (read-only client)", r.Method)
		}
		if r.URL.Path != "/api/v1/namespaces/production/pods" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("labelSelector"); got != "app=alerts-api" {
			t.Errorf("labelSelector = %q, want app=alerts-api", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization header = %q, want configured bearer token", got)
		}
		fmt.Fprint(w, podListFixture)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	pods, err := k.ListPods(context.Background(), "production", "alerts-api")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 2 {
		t.Fatalf("got %d pods, want 2", len(pods))
	}
	if pods[0].Metadata.Name != "alerts-api-6f7d9-x1" {
		t.Errorf("pod name = %q", pods[0].Metadata.Name)
	}
	if imgs := pods[0].Images(); len(imgs) != 1 || imgs[0] != "registry.internal/alerts-api:1.2.3" {
		t.Errorf("images = %v", imgs)
	}
	if pods[1].Status.ContainerStatuses[0].State.Waiting == nil ||
		pods[1].Status.ContainerStatuses[0].State.Waiting.Reason != "CrashLoopBackOff" {
		t.Errorf("waiting state not parsed: %+v", pods[1].Status.ContainerStatuses[0].State)
	}
}

func TestListDeploymentsParsesRolloutState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/apps/v1/namespaces/production/deployments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("labelSelector"); got != "app=alerts-api" {
			t.Errorf("labelSelector = %q", got)
		}
		fmt.Fprint(w, deploymentListFixture)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	deps, err := k.ListDeployments(context.Background(), "production", "alerts-api")
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("got %d deployments, want 1", len(deps))
	}
	d := deps[0]
	if d.DesiredReplicas() != 3 {
		t.Errorf("desired replicas = %d, want 3", d.DesiredReplicas())
	}
	if d.Metadata.Revision() != "14" {
		t.Errorf("revision = %q, want 14", d.Metadata.Revision())
	}
	if d.Status.AvailableReplicas != 3 {
		t.Errorf("available = %d, want 3", d.Status.AvailableReplicas)
	}
	if len(d.Status.Conditions) != 1 || d.Status.Conditions[0].Reason != "MinimumReplicasAvailable" {
		t.Errorf("conditions not parsed: %+v", d.Status.Conditions)
	}
}

func TestKubernetesOmitsAuthorizationWhenNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, "", nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.ListPods(context.Background(), "production", "alerts-api"); err != nil {
		t.Fatalf("ListPods: %v", err)
	}
}

func TestKubernetesDoesNotFollowRedirects(t *testing.T) {
	followed := false
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		if got := r.Header.Get("Authorization"); got != "" {
			t.Error("bearer token leaked to another host")
		}
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer attacker.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer front.Close()

	k, err := NewKubernetes(front.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	_, err = k.ListPods(context.Background(), "production", "alerts-api")
	if err == nil {
		t.Fatal("expected error for redirect response")
	}
	if followed {
		t.Error("client followed the redirect")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaked the token: %v", err)
	}
}

func TestKubernetesErrorBodyDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// A hostile endpoint echoing request headers back must not reach the
		// MCP client alongside our own credentials.
		fmt.Fprintf(w, `{"message":"forbidden: %s"}`, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	_, err = k.ListPods(context.Background(), "production", "alerts-api")
	if err == nil {
		t.Fatal("expected error for 403")
	}
	// The upstream body legitimately contains whatever it contains; assert we
	// at least surface the status and a bounded body.
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should include the status", err)
	}
	if len(err.Error()) > 4096 {
		t.Errorf("error body not bounded: %d bytes", len(err.Error()))
	}
}

func TestKubernetesRejectsInvalidScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request should not have been issued")
		fmt.Fprint(w, `{"items":[]}`)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.ListPods(context.Background(), "Production!", "alerts-api"); err == nil {
		t.Error("invalid namespace should be rejected before any request")
	}
	if _, err := k.ListPods(context.Background(), "production", "OTHER"); err == nil {
		t.Error("invalid service should be rejected before any request")
	}
}

func TestNewKubernetesRejectsPlaintextRemoteEndpoint(t *testing.T) {
	if _, err := NewKubernetes("http://kubernetes.default.svc", testToken, nil, nil); err == nil {
		t.Fatal("expected error for plaintext non-loopback endpoint")
	}
	if _, err := NewKubernetes("https://kubernetes.default.svc", testToken, nil, nil); err != nil {
		t.Fatalf("https endpoint should be accepted: %v", err)
	}
}

func TestKubernetesRejectsMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not-json`)
	}))
	defer srv.Close()

	k, err := NewKubernetes(srv.URL, testToken, nil, nil)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.ListPods(context.Background(), "production", "alerts-api"); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestPodAndDeploymentJSONShapesArePinned(t *testing.T) {
	// Guards against silently depending on fields the fixtures do not set.
	var pods struct {
		Items []Pod `json:"items"`
	}
	if err := json.Unmarshal([]byte(podListFixture), &pods); err != nil {
		t.Fatalf("pod fixture does not unmarshal: %v", err)
	}
	if len(pods.Items) != 2 || pods.Items[0].Metadata.Namespace != "production" {
		t.Errorf("unexpected pod fixture content: %+v", pods.Items)
	}

	var deps struct {
		Items []Deployment `json:"items"`
	}
	if err := json.Unmarshal([]byte(deploymentListFixture), &deps); err != nil {
		t.Fatalf("deployment fixture does not unmarshal: %v", err)
	}
	if deps.Items[0].Spec.Replicas == nil || *deps.Items[0].Spec.Replicas != 3 {
		t.Errorf("replicas not parsed as pointer: %v", deps.Items[0].Spec.Replicas)
	}
}
