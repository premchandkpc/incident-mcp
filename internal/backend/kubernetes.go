package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"incident-mcp/internal/validate"
)

// Kubernetes is a read-only client for a fixed Kubernetes API endpoint. The
// bearer token is attached only to requests issued by this client, which can
// only target the configured base URL.
type Kubernetes struct {
	base   *url.URL
	token  string
	client *http.Client
	log    *log.Logger
}

// NewKubernetes builds a Kubernetes client. token may be empty for anonymous
// (for example in-cluster with no token) access.
func NewKubernetes(rawURL, token string, client *http.Client, logger *log.Logger) (*Kubernetes, error) {
	if err := validate.Endpoint("KUBERNETES_URL", rawURL); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("kubernetes: invalid url: %w", err)
	}
	if client == nil {
		client = NewHTTPClient(0)
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Kubernetes{base: u, token: token, client: client, log: logger}, nil
}

// URL returns the configured base URL (never contains credentials).
func (k *Kubernetes) URL() string { return k.base.String() }

// get performs a read-only GET against a path under the configured API root.
// Only get/list style requests are ever issued by this package.
func (k *Kubernetes) get(ctx context.Context, apiPath string, query url.Values, out any) error {
	u := *k.base
	u.Path = path.Join(strings.TrimSuffix(k.base.Path, "/"), apiPath)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("kubernetes: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}

	k.log.Printf("kubernetes GET %s?%s", apiPath, query.Encode())
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kubernetes: GET %s: %s: %s", apiPath, resp.Status, readErrorBody(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("kubernetes: decode response from %s: %w", apiPath, err)
	}
	return nil
}

// ObjectMeta is the subset of Kubernetes object metadata this server uses.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
}

// Revision returns the deployment revision annotation when present.
func (m ObjectMeta) Revision() string {
	if m.Annotations == nil {
		return ""
	}
	return m.Annotations["deployment.kubernetes.io/revision"]
}

// ContainerSpec identifies a container image in a pod template.
type ContainerSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// ContainerState mirrors the Kubernetes container state union.
type ContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason     string    `json:"reason"`
		Message    string    `json:"message"`
		ExitCode   int       `json:"exitCode"`
		FinishedAt time.Time `json:"finishedAt"`
	} `json:"terminated"`
}

// ContainerStatus mirrors the Kubernetes container status object.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int            `json:"restartCount"`
	Image        string         `json:"image"`
	State        ContainerState `json:"state"`
	LastState    ContainerState `json:"lastState"`
}

// PodCondition mirrors the Kubernetes pod condition object.
type PodCondition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// PodStatus mirrors the Kubernetes pod status object.
type PodStatus struct {
	Phase                 string            `json:"phase"`
	Reason                string            `json:"reason"`
	Message               string            `json:"message"`
	StartTime             *time.Time        `json:"startTime"`
	NodeName              string            `json:"nodeName"`
	Conditions            []PodCondition    `json:"conditions"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses"`
}

// Pod is a Kubernetes pod with only the fields needed for failure triage.
type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		NodeName   string          `json:"nodeName"`
		Containers []ContainerSpec `json:"containers"`
	} `json:"spec"`
	Status PodStatus `json:"status"`
}

// Images returns the pod's container images.
func (p Pod) Images() []string {
	var imgs []string
	for _, c := range p.Spec.Containers {
		if c.Image != "" {
			imgs = append(imgs, c.Image)
		}
	}
	return imgs
}

// DeploymentCondition mirrors a Kubernetes deployment condition.
type DeploymentCondition struct {
	Type           string    `json:"type"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	LastUpdateTime time.Time `json:"lastUpdateTime"`
}

// Deployment is a Kubernetes Deployment with only the fields needed for
// rollout correlation.
type Deployment struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
		Selector *struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Spec struct {
				Containers []ContainerSpec `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas           int                   `json:"replicas"`
		UpdatedReplicas    int                   `json:"updatedReplicas"`
		ReadyReplicas      int                   `json:"readyReplicas"`
		AvailableReplicas  int                   `json:"availableReplicas"`
		ObservedGeneration int64                 `json:"observedGeneration"`
		Conditions         []DeploymentCondition `json:"conditions"`
	} `json:"status"`
}

// Images returns the deployment's container images.
func (d Deployment) Images() []string {
	var imgs []string
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Image != "" {
			imgs = append(imgs, c.Image)
		}
	}
	return imgs
}

// DesiredReplicas returns spec.replicas, defaulting to 1 when unset, as the
// Kubernetes API does.
func (d Deployment) DesiredReplicas() int {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// ListPods lists pods in the given namespace selected by app=<service>.
func (k *Kubernetes) ListPods(ctx context.Context, namespace, service string) ([]Pod, error) {
	if err := validate.Namespace(namespace); err != nil {
		return nil, err
	}
	if err := validate.Service(service, ""); err != nil {
		return nil, err
	}
	var list struct {
		Items []Pod `json:"items"`
	}
	q := url.Values{"labelSelector": []string{"app=" + service}}
	if err := k.get(ctx, "/api/v1/namespaces/"+namespace+"/pods", q, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// ListDeployments lists Deployments in the given namespace selected by
// app=<service>.
func (k *Kubernetes) ListDeployments(ctx context.Context, namespace, service string) ([]Deployment, error) {
	if err := validate.Namespace(namespace); err != nil {
		return nil, err
	}
	if err := validate.Service(service, ""); err != nil {
		return nil, err
	}
	var list struct {
		Items []Deployment `json:"items"`
	}
	q := url.Values{"labelSelector": []string{"app=" + service}}
	if err := k.get(ctx, "/apis/apps/v1/namespaces/"+namespace+"/deployments", q, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}
