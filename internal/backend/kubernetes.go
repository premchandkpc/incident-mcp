package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"incident-mcp/internal/validate"
)

// Page-size bounds for every list call. The limit is always sent upstream so
// a single response can never grow without bound, and callers can request at
// most MaxListLimit items per page.
const (
	DefaultListLimit = 50
	MaxListLimit     = 500
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

// listMeta is the `metadata` object of a Kubernetes list response.
type listMeta struct {
	Continue string `json:"continue"`
}

// ListOptions bounds a single list request. A zero Limit selects
// DefaultListLimit; Continue resumes a previous page.
type ListOptions struct {
	Limit    int
	Continue string
}

// clampLimit applies the page-size bounds. The limit is always sent to the
// API server so one response can never fill memory regardless of caller.
func clampLimit(n int) int {
	if n <= 0 {
		return DefaultListLimit
	}
	if n > MaxListLimit {
		return MaxListLimit
	}
	return n
}

// Page is one bounded page of a list response. A non-empty Continue means
// more items remain upstream.
type Page[T any] struct {
	Items     []T    `json:"items"`
	Continue  string `json:"-"`
	Truncated bool   `json:"-"`
	Limit     int    `json:"-"`
}

// PageInfo renders the paging state for tool output.
func (p Page[T]) PageInfo() map[string]any {
	info := map[string]any{
		"requested_limit": p.Limit,
		"returned":        len(p.Items),
		"has_more":        p.Continue != "",
	}
	if p.Continue != "" {
		info["next_continue"] = p.Continue
	}
	return info
}

// get performs a read-only GET against a path under the configured API root
// and decodes a Kubernetes list body, returning its paging metadata. Only
// get/list style requests are ever issued by this package.
func (k *Kubernetes) get(ctx context.Context, apiPath string, query url.Values, items any) (listMeta, error) {
	u := *k.base
	u.Path = path.Join(strings.TrimSuffix(k.base.Path, "/"), apiPath)
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return listMeta{}, fmt.Errorf("kubernetes: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}

	k.log.Printf("kubernetes GET %s?%s", apiPath, query.Encode())
	resp, err := k.client.Do(req)
	if err != nil {
		return listMeta{}, fmt.Errorf("kubernetes: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return listMeta{}, fmt.Errorf("kubernetes: GET %s: %s: %s", apiPath, resp.Status, readErrorBody(resp.Body))
	}
	var body struct {
		Metadata listMeta        `json:"metadata"`
		Items    json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return listMeta{}, fmt.Errorf("kubernetes: decode response from %s: %w", apiPath, err)
	}
	if items != nil && len(body.Items) > 0 {
		if err := json.Unmarshal(body.Items, items); err != nil {
			return listMeta{}, fmt.Errorf("kubernetes: decode items from %s: %w", apiPath, err)
		}
	}
	return body.Metadata, nil
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

// ListPods returns one bounded page of pods in the given namespace selected
// by app=<service>.
func (k *Kubernetes) ListPods(ctx context.Context, namespace, service string, opts ListOptions) (Page[Pod], error) {
	if err := validate.Namespace(namespace); err != nil {
		return Page[Pod]{}, err
	}
	if err := validate.Service(service, ""); err != nil {
		return Page[Pod]{}, err
	}
	limit := clampLimit(opts.Limit)
	items := []Pod{}
	q := url.Values{
		"labelSelector": []string{"app=" + service},
		"limit":         []string{strconv.Itoa(limit)},
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	meta, err := k.get(ctx, "/api/v1/namespaces/"+namespace+"/pods", q, &items)
	if err != nil {
		return Page[Pod]{}, err
	}
	return Page[Pod]{Items: items, Continue: meta.Continue, Truncated: meta.Continue != "", Limit: limit}, nil
}

// ListDeployments returns one bounded page of Deployments in the given
// namespace selected by app=<service>.
func (k *Kubernetes) ListDeployments(ctx context.Context, namespace, service string, opts ListOptions) (Page[Deployment], error) {
	if err := validate.Namespace(namespace); err != nil {
		return Page[Deployment]{}, err
	}
	if err := validate.Service(service, ""); err != nil {
		return Page[Deployment]{}, err
	}
	limit := clampLimit(opts.Limit)
	items := []Deployment{}
	q := url.Values{
		"labelSelector": []string{"app=" + service},
		"limit":         []string{strconv.Itoa(limit)},
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	meta, err := k.get(ctx, "/apis/apps/v1/namespaces/"+namespace+"/deployments", q, &items)
	if err != nil {
		return Page[Deployment]{}, err
	}
	return Page[Deployment]{Items: items, Continue: meta.Continue, Truncated: meta.Continue != "", Limit: limit}, nil
}

// ReplicaSet is the subset of a Kubernetes ReplicaSet used to reconstruct
// rollout history: each rollout creates a ReplicaSet carrying the revision
// annotation and the pod template that produced it.
type ReplicaSet struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []ContainerSpec `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas           int   `json:"replicas"`
		ReadyReplicas      int   `json:"readyReplicas"`
		AvailableReplicas  int   `json:"availableReplicas"`
		ObservedGeneration int64 `json:"observedGeneration"`
	} `json:"status"`
}

// Revision returns the deployment revision annotation when present.
func (r ReplicaSet) Revision() string { return r.Metadata.Revision() }

// Images returns the ReplicaSet's container images.
func (r ReplicaSet) Images() []string {
	var imgs []string
	for _, c := range r.Spec.Template.Spec.Containers {
		if c.Image != "" {
			imgs = append(imgs, c.Image)
		}
	}
	return imgs
}

// DesiredReplicas returns spec.replicas, defaulting to 1 when unset.
func (r ReplicaSet) DesiredReplicas() int {
	if r.Spec.Replicas == nil {
		return 1
	}
	return *r.Spec.Replicas
}

// ListReplicaSets returns one bounded page of ReplicaSets in the namespace
// selected by app=<service>, which is the durable record of past rollouts.
func (k *Kubernetes) ListReplicaSets(ctx context.Context, namespace, service string, opts ListOptions) (Page[ReplicaSet], error) {
	if err := validate.Namespace(namespace); err != nil {
		return Page[ReplicaSet]{}, err
	}
	if err := validate.Service(service, ""); err != nil {
		return Page[ReplicaSet]{}, err
	}
	limit := clampLimit(opts.Limit)
	items := []ReplicaSet{}
	q := url.Values{
		"labelSelector": []string{"app=" + service},
		"limit":         []string{strconv.Itoa(limit)},
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	meta, err := k.get(ctx, "/apis/apps/v1/namespaces/"+namespace+"/replicasets", q, &items)
	if err != nil {
		return Page[ReplicaSet]{}, err
	}
	return Page[ReplicaSet]{Items: items, Continue: meta.Continue, Truncated: meta.Continue != "", Limit: limit}, nil
}

// Event is the subset of a Kubernetes Event used for incident correlation.
type Event struct {
	Metadata       ObjectMeta `json:"metadata"`
	Type           string     `json:"type"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Count          int        `json:"count"`
	FirstTimestamp time.Time  `json:"firstTimestamp"`
	LastTimestamp  time.Time  `json:"lastTimestamp"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"involvedObject"`
}

// Timestamp returns the most recent time the event was observed.
func (e Event) Timestamp() time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp
	}
	return e.Metadata.CreationTimestamp
}

// ListEvents returns one bounded page of namespace Events. fieldSelector is
// passed through verbatim by the caller; it must be built from validated
// identifiers only.
func (k *Kubernetes) ListEvents(ctx context.Context, namespace string, fieldSelector string, opts ListOptions) (Page[Event], error) {
	if err := validate.Namespace(namespace); err != nil {
		return Page[Event]{}, err
	}
	limit := clampLimit(opts.Limit)
	items := []Event{}
	q := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if fieldSelector != "" {
		q.Set("fieldSelector", fieldSelector)
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	meta, err := k.get(ctx, "/api/v1/namespaces/"+namespace+"/events", q, &items)
	if err != nil {
		return Page[Event]{}, err
	}
	return Page[Event]{Items: items, Continue: meta.Continue, Truncated: meta.Continue != "", Limit: limit}, nil
}
