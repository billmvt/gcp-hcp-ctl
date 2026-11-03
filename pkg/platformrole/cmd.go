// Package platformrole provides read-only discovery of the platform role catalog.
package platformrole

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/config"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/discovery"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/internal/access"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/output"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type clientKey struct{}

type regionResolver interface {
	Region(context.Context, *config.Config) (discovery.RegionInfo, error)
}

// NewPlatformRoleCmd returns the read-only platformrole command group.
func NewPlatformRoleCmd() *cobra.Command { return newCommand(discovery.NewResolver()) }

// newCommand builds the read-only catalog group and resolves its endpoint independently of
// project context.
func newCommand(resolver regionResolver) *cobra.Command {
	group := &cobra.Command{
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		Use:   "platformrole",
		Short: "Discover platform roles and their permissions",
		Long:  "List and inspect system-managed platform roles. All authenticated users can read the catalog. Project values from flags, environment, and config are accepted but ignored; --env and --region select the service instance. Role definitions are managed privately, not through this command.",
	}
	group.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if parent := group.Parent(); parent != nil && parent.PersistentPreRunE != nil {
			if err := parent.PersistentPreRunE(cmd, args); err != nil {
				return err
			}
		}
		if cmd == group {
			return nil
		}
		endpoint, _ := cmd.Flags().GetString("api-endpoint")
		environment, _ := cmd.Flags().GetString("environment")
		region, _ := cmd.Flags().GetString("region")
		// API-only discovery must not fetch an OIDC issuer when an explicit API
		// endpoint is already available, including during discovery outages.
		if strings.TrimSpace(endpoint) == "" {
			info, err := resolver.Region(cmd.Context(), &config.Config{Environment: environment, Region: region})
			if err != nil {
				return err
			}
			endpoint = info.PlatformAPIEndpoint
		}
		if strings.TrimSpace(endpoint) == "" {
			return discovery.EndpointRequiredError(discovery.KindAPI, environment)
		}
		if access.Client(cmd) != nil {
			return nil
		}
		client, err := platformapi.NewPlatformRoleClient(endpoint, auth.NewTokenSource(auth.PlatformAPIAudience))
		if err != nil {
			return err
		}
		cmd.SetContext(context.WithValue(cmd.Context(), clientKey{}, client))
		return nil
	}
	group.AddCommand(newListCmd(), newGetCmd())
	return group
}

// client selects an injected test client or the authenticated catalog client created during
// pre-run.
func client(cmd *cobra.Command) platformapi.PlatformRoleInterface {
	if c := access.Client(cmd); c != nil {
		return c.PlatformRoles()
	}
	return cmd.Context().Value(clientKey{}).(platformapi.PlatformRoleInterface)
}

// prepareRole applies the catalog's minimal CLI output contract without mutating
// the API response. In particular, do not surface managedFields or GitOps data.
func prepareRole(role *platformapi.PlatformRole) *platformapi.PlatformRole {
	role = role.DeepCopy()
	role.TypeMeta = metav1.TypeMeta{APIVersion: gcpv1.GroupVersion.String(), Kind: "PlatformRole"}
	role.ObjectMeta = metav1.ObjectMeta{Name: role.Name, CreationTimestamp: role.CreationTimestamp}
	return role
}

// newListCmd aggregates the catalog and prints stable, minimal metadata without mutating
// API responses.
func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List platform roles (project is ignored)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			roles, err := client(cmd).List(cmd.Context())
			if err != nil {
				return fmt.Errorf("listing platform roles: %w", err)
			}
			roles = roles.DeepCopy()
			roles.TypeMeta = metav1.TypeMeta{APIVersion: gcpv1.GroupVersion.String(), Kind: "PlatformRoleList"}
			roles.Continue = ""
			roles.RemainingItemCount = nil
			if roles.Items == nil {
				roles.Items = []platformapi.PlatformRole{}
			}
			for i := range roles.Items {
				roles.Items[i] = *prepareRole(&roles.Items[i])
			}
			sort.SliceStable(roles.Items, func(i, j int) bool { return roles.Items[i].Name < roles.Items[j].Name })
			if access.Format(cmd) != output.FormatText {
				return access.Structured(cmd, roles)
			}
			table := output.NewTable(cmd.OutOrStdout(), "NAME", "PERMISSIONS", "AGE")
			for _, role := range roles.Items {
				table.AddRow(role.Name, strconv.Itoa(len(role.Spec.Permissions)), access.Age(role.CreationTimestamp))
			}
			return table.Flush()
		},
	}
}

// newGetCmd prints one catalog entry with every permission and only public CLI metadata.
func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get a platform role and all its permissions (project is ignored)",
		Args:  access.Named,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := client(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("getting platform role: %w", err)
			}
			role = prepareRole(role)
			if access.Format(cmd) != output.FormatText {
				return access.Structured(cmd, role)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Name: %s\nAge: %s\nPermissions:\n", role.Name, access.Age(role.CreationTimestamp)); err != nil {
				return err
			}
			for _, permission := range role.Spec.Permissions {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", permission); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
