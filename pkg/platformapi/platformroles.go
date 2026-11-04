package platformapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"k8s.io/client-go/rest"
)

// PlatformRoleInterface exposes only platform-scoped catalog reads.
type PlatformRoleInterface interface {
	List(context.Context) (*PlatformRoleList, error)
	Get(context.Context, string) (*PlatformRole, error)
}

// Bound requests and token tracking if a broken endpoint keeps returning unique
// continuation tokens. Hitting the ceiling must not return a partial catalog.
const maxPlatformRoleListPages = 100

type platformRoleClient struct{ restClient rest.Interface }

// PlatformRoles returns a read-only client, independent of the configured project.
func (c *Client) PlatformRoles() PlatformRoleInterface {
	return &platformRoleClient{restClient: c.restClient}
}

// Get fetches one platform-scoped catalog entry and preserves centralized safe error
// normalization.
func (c *platformRoleClient) Get(ctx context.Context, name string) (*PlatformRole, error) {
	result := &PlatformRole{}
	response := c.restClient.Get().
		Resource("platformroles").
		Name(name).
		Do(ctx)
	if err := decodeResult(response, result, http.MethodGet, "platformroles", name, false); err != nil {
		return nil, err
	}
	result.SetGroupVersionKind(gcpv1.GroupVersion.WithKind("PlatformRole"))
	return result, nil
}

// List returns the complete platform-scoped platform-role catalog in at most
// 100 pages. It returns an error, not partial results, if the limit is exceeded.
func (c *platformRoleClient) List(ctx context.Context) (*PlatformRoleList, error) {
	var result *PlatformRoleList
	token := ""
	seen := map[string]bool{}
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber == maxPlatformRoleListPages {
			return nil, errors.New("pagination limit exceeded")
		}
		page := &PlatformRoleList{}
		request := c.restClient.Get().Resource("platformroles")
		if token != "" {
			request = request.Param("continue", token)
		}
		if err := decodeResult(request.Do(ctx), page, http.MethodGet, "platformroles", "", false); err != nil {
			return nil, err
		}
		if result == nil {
			result = page
		} else {
			result.Items = append(result.Items, page.Items...)
		}
		token = page.Continue
		if token == "" {
			break
		}
		if seen[token] {
			return nil, errors.New("repeated pagination token")
		}
		seen[token] = true
	}
	result.Continue = ""
	result.RemainingItemCount = nil
	if result.Items == nil {
		result.Items = []PlatformRole{}
	}
	result.SetGroupVersionKind(gcpv1.GroupVersion.WithKind("PlatformRoleList"))
	return result, nil
}

// NewPlatformRoleClient creates an authenticated, project-independent catalog client.
// Only read operations are exposed; namespaced clients still require a project.
func NewPlatformRoleClient(endpoint string, tokenSource *auth.TokenSource) (PlatformRoleInterface, error) {
	c, err := newClient(endpoint, "", tokenSource)
	if err != nil {
		return nil, err
	}
	return c.PlatformRoles(), nil
}
