// Package platformapi provides a typed REST client for the platform-api-server,
// which serves Kubernetes-style resources under the gcp.managed.openshift.io API group.
package platformapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/httpdebug"

	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

func init() {
	if err := gcpv1.AddToScheme(scheme); err != nil {
		panic(fmt.Sprintf("registering platform-api types: %v", err))
	}
}

// Client wraps a rest.RESTClient for the platform-api-server.
// The project (GCP project ID) is stored once at construction and used as
// the Kubernetes namespace for all scoped operations.
type Client struct {
	restClient rest.Interface
	project    string
}

// NewClient creates a platform-api client from an API endpoint URL, project ID, and token source.
// The project is used as the Kubernetes namespace for all scoped operations.
func NewClient(apiEndpoint, project string, tokenSource *auth.TokenSource) (*Client, error) {
	if project == "" {
		return nil, fmt.Errorf("project is required (set --project, GCPHCPCTL_PROJECT, or project in config)")
	}
	return newClient(apiEndpoint, project, tokenSource)
}

// newClient shares HTTPS/authenticated transport setup with the read-only catalog client.
func newClient(apiEndpoint, project string, tokenSource *auth.TokenSource) (*Client, error) {
	if tokenSource == nil {
		return nil, fmt.Errorf("token source is required")
	}
	if !strings.HasPrefix(apiEndpoint, "https://") {
		return nil, fmt.Errorf("API endpoint must use HTTPS")
	}

	apiEndpoint = strings.TrimRight(apiEndpoint, "/")

	cfg := clientRESTConfig(apiEndpoint)
	// Authentication stays outside tracing, so token failures are not sent attempts.
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &tokenTransport{base: rt, tokenSource: tokenSource}
	})

	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		// Parser errors can contain endpoint hosts, userinfo and query credentials.
		return nil, fmt.Errorf("creating REST client: invalid API endpoint configuration")
	}

	return &Client{restClient: rc, project: project}, nil
}

func clientRESTConfig(endpoint string) *rest.Config {
	return &rest.Config{
		WrapTransport: httpdebug.Wrap,
		Host:          endpoint,
		APIPath:       "/apis",
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &gcpv1.GroupVersion,
			NegotiatedSerializer: codecs.WithoutConversion(),
		},
	}
}

// NewClientForTest builds a credential-free client for a local HTTP test server.
// It must never be used for production requests: unlike NewClient, it does not
// enforce HTTPS or install an authentication transport. Client-side throttling
// is disabled only for this loopback-only test helper.
func NewClientForTest(rawURL, project string) (*Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("test client requires a loopback HTTP endpoint")
	}
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return nil, fmt.Errorf("test client requires a loopback HTTP endpoint")
	}
	cfg := clientRESTConfig(rawURL)
	cfg.QPS = -1 // Loopback-only test clients must not wait on request throttling.
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating REST client: %w", err)
	}
	return &Client{restClient: rc, project: project}, nil
}

// tokenTransport injects an Authorization header using the auth.TokenSource.
type tokenTransport struct {
	base        http.RoundTripper
	tokenSource interface {
		Token(context.Context) (string, string, error)
	}
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, _, err := t.tokenSource.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("obtaining auth token: %w", err)
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(req)
}

// Project returns the GCP project ID (Kubernetes namespace) this client is scoped to.
func (c *Client) Project() string {
	return c.project
}

// Namespace returns the Kubernetes namespace for the configured project.
func (c *Client) Namespace() string {
	return NamespaceForProject(c.project)
}

// Clusters returns a ClusterInterface for performing cluster operations.
func (c *Client) Clusters() ClusterInterface {
	return &clusterClient{restClient: c.restClient}
}

// NodePools returns a NodePoolInterface for performing nodepool operations.
func (c *Client) NodePools() NodePoolInterface {
	return &nodePoolClient{restClient: c.restClient}
}

// Versions returns a VersionInterface for performing version operations.
func (c *Client) Versions() VersionInterface {
	return &versionClient{restClient: c.restClient}
}

// ControlPlaneUpgradePolicies returns a ControlPlaneUpgradePolicyInterface for
// performing control-plane upgrade policy operations.
func (c *Client) ControlPlaneUpgradePolicies() ControlPlaneUpgradePolicyInterface {
	return &controlPlaneUpgradePolicyClient{restClient: c.restClient}
}

// Channels returns a ChannelInterface for performing channel operations.
func (c *Client) Channels() ChannelInterface {
	return &channelClient{restClient: c.restClient}
}

// ClusterInterface defines operations on Cluster resources.
type ClusterInterface interface {
	Create(ctx context.Context, namespace string, cluster *gcpv1.Cluster) (*gcpv1.Cluster, error)
	Get(ctx context.Context, namespace, name string) (*gcpv1.Cluster, error)
	List(ctx context.Context, namespace string) (*gcpv1.ClusterList, error)
	Delete(ctx context.Context, namespace, name string) error
}

type clusterClient struct {
	restClient rest.Interface
}

func (c *clusterClient) Create(ctx context.Context, namespace string, cluster *gcpv1.Cluster) (*gcpv1.Cluster, error) {
	result := &gcpv1.Cluster{}
	response := c.restClient.Post().
		MaxRetries(0).
		Namespace(namespace).
		Resource("clusters").
		Body(cluster).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodPost, "clusters", cluster.Name, true); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *clusterClient) Get(ctx context.Context, namespace, name string) (*gcpv1.Cluster, error) {
	result := &gcpv1.Cluster{}
	response := c.restClient.Get().
		Namespace(namespace).
		Resource("clusters").
		Name(name).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "clusters", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

// List returns clusters in the given namespace.
func (c *clusterClient) List(ctx context.Context, namespace string) (*gcpv1.ClusterList, error) {
	result := &gcpv1.ClusterList{}
	response := c.restClient.Get().
		Namespace(namespace).
		Resource("clusters").
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "clusters", "", false); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *clusterClient) Delete(ctx context.Context, namespace, name string) error {
	response := c.restClient.Delete().
		MaxRetries(0).
		Namespace(namespace).
		Resource("clusters").
		Name(name).
		Do(ctx)
	return normalizeResult(response, http.MethodDelete, "clusters", name, false)
}

// VersionInterface defines operations on cluster-scoped Version resources.
type VersionInterface interface {
	Get(ctx context.Context, name string) (*gcpv1.Version, error)
	List(ctx context.Context) (*gcpv1.VersionList, error)
}

type versionClient struct {
	restClient rest.Interface
}

func (v *versionClient) Get(ctx context.Context, name string) (*gcpv1.Version, error) {
	result := &gcpv1.Version{}
	response := v.restClient.Get().
		Resource("versions").
		Name(name).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "versions", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (v *versionClient) List(ctx context.Context) (*gcpv1.VersionList, error) {
	result := &gcpv1.VersionList{}
	response := v.restClient.Get().
		Resource("versions").
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "versions", "", false); err != nil {
		return nil, err
	}
	return result, nil
}

// ChannelInterface defines operations on cluster-scoped Channel resources.
type ChannelInterface interface {
	List(ctx context.Context) (*gcpv1.ChannelList, error)
}

type channelClient struct {
	restClient rest.Interface
}

func (c *channelClient) List(ctx context.Context) (*gcpv1.ChannelList, error) {
	result := &gcpv1.ChannelList{}
	response := c.restClient.Get().
		Resource("channels").
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "channels", "", false); err != nil {
		return nil, err
	}
	return result, nil
}

// ResolveCluster finds a cluster by name within the client's project namespace.
func (c *Client) ResolveCluster(ctx context.Context, name string) (*gcpv1.Cluster, error) {
	cluster, err := c.Clusters().Get(ctx, c.Namespace(), name)
	if err != nil {
		return nil, fmt.Errorf("looking up cluster %q in project %q: %w", name, c.project, err)
	}
	return cluster, nil
}

// ResolveNodePool finds a nodepool by name within the client's project namespace.
func (c *Client) ResolveNodePool(ctx context.Context, name string) (*gcpv1.NodePool, error) {
	nodePool, err := c.NodePools().Get(ctx, c.Namespace(), name)
	if err != nil {
		return nil, fmt.Errorf("looking up nodepool %q in project %q: %w", name, c.project, err)
	}
	return nodePool, nil
}

// NodePoolInterface defines operations on NodePool resources.
type NodePoolInterface interface {
	Create(ctx context.Context, namespace string, nodePool *gcpv1.NodePool) (*gcpv1.NodePool, error)
	Get(ctx context.Context, namespace, name string) (*gcpv1.NodePool, error)
	List(ctx context.Context, namespace string) (*gcpv1.NodePoolList, error)
	Patch(ctx context.Context, namespace, name string, patchData []byte) (*gcpv1.NodePool, error)
	Delete(ctx context.Context, namespace, name string) error
}

type nodePoolClient struct {
	restClient rest.Interface
}

func (n *nodePoolClient) Create(ctx context.Context, namespace string, nodePool *gcpv1.NodePool) (*gcpv1.NodePool, error) {
	result := &gcpv1.NodePool{}
	response := n.restClient.Post().
		MaxRetries(0).
		Namespace(namespace).
		Resource("nodepools").
		Body(nodePool).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodPost, "nodepools", nodePool.Name, true); err != nil {
		return nil, err
	}
	return result, nil
}

func (n *nodePoolClient) Get(ctx context.Context, namespace, name string) (*gcpv1.NodePool, error) {
	result := &gcpv1.NodePool{}
	response := n.restClient.Get().
		Namespace(namespace).
		Resource("nodepools").
		Name(name).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "nodepools", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (n *nodePoolClient) List(ctx context.Context, namespace string) (*gcpv1.NodePoolList, error) {
	result := &gcpv1.NodePoolList{}
	response := n.restClient.Get().
		Namespace(namespace).
		Resource("nodepools").
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "nodepools", "", false); err != nil {
		return nil, err
	}
	return result, nil
}

func (n *nodePoolClient) Patch(ctx context.Context, namespace, name string, patchData []byte) (*gcpv1.NodePool, error) {
	result := &gcpv1.NodePool{}
	response := n.restClient.Patch(types.MergePatchType).
		MaxRetries(0).
		Namespace(namespace).
		Resource("nodepools").
		Name(name).
		Body(patchData).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodPatch, "nodepools", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

func (n *nodePoolClient) Delete(ctx context.Context, namespace, name string) error {
	response := n.restClient.Delete().
		MaxRetries(0).
		Namespace(namespace).
		Resource("nodepools").
		Name(name).
		Do(ctx)
	return normalizeResult(response, http.MethodDelete, "nodepools", name, false)
}

// ControlPlaneUpgradePolicyInterface defines operations on
// ControlPlaneUpgradePolicy resources.
type ControlPlaneUpgradePolicyInterface interface {
	Create(ctx context.Context, namespace string, policy *gcpv1.ControlPlaneUpgradePolicy) (*gcpv1.ControlPlaneUpgradePolicy, error)
	Get(ctx context.Context, namespace, name string) (*gcpv1.ControlPlaneUpgradePolicy, error)
	Patch(ctx context.Context, namespace, name string, patchData []byte) (*gcpv1.ControlPlaneUpgradePolicy, error)
	Delete(ctx context.Context, namespace, name string) error
}

type controlPlaneUpgradePolicyClient struct {
	restClient rest.Interface
}

// Create posts a policy without retries and reports uncertain write outcomes.
func (c *controlPlaneUpgradePolicyClient) Create(ctx context.Context, namespace string, policy *gcpv1.ControlPlaneUpgradePolicy) (*gcpv1.ControlPlaneUpgradePolicy, error) {
	result := &gcpv1.ControlPlaneUpgradePolicy{}
	response := c.restClient.Post().
		MaxRetries(0).
		Namespace(namespace).
		Resource("controlplaneupgradepolicies").
		Body(policy).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodPost, "controlplaneupgradepolicies", policy.Name, true); err != nil {
		return nil, err
	}
	return result, nil
}

// Get retrieves a policy and normalizes request and response decoding errors.
func (c *controlPlaneUpgradePolicyClient) Get(ctx context.Context, namespace, name string) (*gcpv1.ControlPlaneUpgradePolicy, error) {
	result := &gcpv1.ControlPlaneUpgradePolicy{}
	response := c.restClient.Get().
		Namespace(namespace).
		Resource("controlplaneupgradepolicies").
		Name(name).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "controlplaneupgradepolicies", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

// Patch applies a JSON Merge Patch without retries and returns the updated policy.
func (c *controlPlaneUpgradePolicyClient) Patch(ctx context.Context, namespace, name string, patchData []byte) (*gcpv1.ControlPlaneUpgradePolicy, error) {
	result := &gcpv1.ControlPlaneUpgradePolicy{}
	response := c.restClient.Patch(types.MergePatchType).
		MaxRetries(0).
		Namespace(namespace).
		Resource("controlplaneupgradepolicies").
		Name(name).
		Body(patchData).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodPatch, "controlplaneupgradepolicies", name, false); err != nil {
		return nil, err
	}
	return result, nil
}

// Delete removes a policy without retries and normalizes API errors.
func (c *controlPlaneUpgradePolicyClient) Delete(ctx context.Context, namespace, name string) error {
	response := c.restClient.Delete().
		MaxRetries(0).
		Namespace(namespace).
		Resource("controlplaneupgradepolicies").
		Name(name).
		Do(ctx)
	return normalizeResult(response, http.MethodDelete, "controlplaneupgradepolicies", name, false)
}

// NamespaceForProject returns the namespace for a given GCP project ID.
// The platform API server uses a per-project multi-tenancy model where
// the namespace equals the project ID.
func NamespaceForProject(projectID string) string {
	return projectID
}
