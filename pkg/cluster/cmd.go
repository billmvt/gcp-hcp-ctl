package cluster

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/config"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/discovery"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/output"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type contextKey string

const (
	clientKey     contextKey = "platform-api-client"
	regionInfoKey contextKey = "discovery-region-info"
)

// NewClusterCmd returns the "cluster" command group. effective carries the
// resolved configuration (including the discovered platform API endpoint),
// populated by the root command's PersistentPreRunE before any subcommand runs.
func NewClusterCmd(effective *config.Config) *cobra.Command {
	var clusterCmd *cobra.Command
	clusterCmd = &cobra.Command{
		Use:   "cluster",
		Short: "Manage GCP HCP clusters",
		Long:  `Create, get, list, delete, and log in to GCP HCP clusters via the platform API server.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if parent := clusterCmd.Parent(); parent != nil && parent.PersistentPreRunE != nil {
				if err := parent.PersistentPreRunE(cmd, args); err != nil {
					return err
				}
			}
			// Resolve the region's endpoints once for the whole cluster group and
			// stash them in the context alongside the client.
			info, err := discovery.NewResolver().Region(cmd.Context(), effective)
			if err != nil {
				return err
			}
			if info.PlatformAPIEndpoint == "" {
				return discovery.EndpointRequiredError(discovery.KindAPI, effective.Environment)
			}
			client, err := newClient(info.PlatformAPIEndpoint, effective.Project)
			if err != nil {
				return err
			}
			ctx := context.WithValue(cmd.Context(), clientKey, client)
			ctx = context.WithValue(ctx, regionInfoKey, info)
			cmd.SetContext(ctx)
			return nil
		},
	}

	clusterCmd.AddCommand(newCreateCmd())
	clusterCmd.AddCommand(newGetCmd())
	clusterCmd.AddCommand(newListCmd())
	clusterCmd.AddCommand(newDeleteCmd())
	clusterCmd.AddCommand(newLoginCmd())
	clusterCmd.AddCommand(newSetUpgradePolicyCmd())
	clusterCmd.AddCommand(newClearUpgradePolicyCmd())

	return clusterCmd
}

func newClient(apiEndpoint, project string) (*platformapi.Client, error) {
	return platformapi.NewClient(apiEndpoint, project, auth.NewTokenSource(auth.PlatformAPIAudience))
}

func clientFromCmd(cmd *cobra.Command) *platformapi.Client {
	client, ok := cmd.Context().Value(clientKey).(*platformapi.Client)
	if !ok {
		panic("bug: clientFromCmd called before PersistentPreRunE set the platform API client")
	}
	return client
}

// regionInfoFromCmd returns the region endpoints stashed by the cluster group's
// PersistentPreRunE. The API client and cluster create use this same result.
func regionInfoFromCmd(cmd *cobra.Command) discovery.RegionInfo {
	info, _ := cmd.Context().Value(regionInfoKey).(discovery.RegionInfo)
	return info
}

func printCluster(w io.Writer, c *gcpv1.Cluster, format string) error {
	switch output.ParseFormat(format) {
	case output.FormatJSON:
		return output.PrintJSON(w, c)
	case output.FormatYAML:
		return output.PrintYAML(w, c)
	default:
	}

	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "Name:            %s\n", c.Name)
	fmt.Fprintf(bw, "ID:              %s\n", c.UID)
	if c.Spec.InfraID != "" {
		fmt.Fprintf(bw, "Infra ID:        %s\n", c.Spec.InfraID)
	}
	fmt.Fprintf(bw, "Status:          %s\n", clusterStatusDetail(c))
	if !c.CreationTimestamp.IsZero() {
		fmt.Fprintf(bw, "Created:         %s (%s)\n",
			c.CreationTimestamp.UTC().Format(time.RFC3339),
			output.Age(c.CreationTimestamp.UTC().Format(time.RFC3339)))
	}

	if c.Spec.Release.Version != "" {
		ver := c.Spec.Release.Version
		if c.Spec.Release.ChannelGroup != "" {
			ver = fmt.Sprintf("%s (%s)", ver, c.Spec.Release.ChannelGroup)
		}
		fmt.Fprintf(bw, "Version:         %s\n", ver)
	}

	if hcr := c.Status.HostedClusterResult; hcr != nil {
		if hcr.APIEndpoint != "" {
			fmt.Fprintf(bw, "API Endpoint:    %s\n", hcr.APIEndpoint)
		}
		if hcr.Version != "" {
			fmt.Fprintf(bw, "HC Version:      %s\n", hcr.Version)
		}
	}

	if gcp := c.Spec.Platform.GCP; gcp != nil {
		fmt.Fprintln(bw, "\nPlatform:")
		fmt.Fprintf(bw, "  Provider:      GCP\n")
		fmt.Fprintf(bw, "  Project:       %s\n", gcp.ProjectID)
		fmt.Fprintf(bw, "  Region:        %s\n", gcp.Region)
		if gcp.EndpointAccess != "" {
			fmt.Fprintf(bw, "  Access:        %s\n", gcp.EndpointAccess)
		}
		if gcp.Network != "" {
			fmt.Fprintf(bw, "  Network:       %s\n", gcp.Network)
		}
		if gcp.Subnet != "" {
			fmt.Fprintf(bw, "  Subnet:        %s\n", gcp.Subnet)
		}
	}

	net := c.Spec.Networking
	if net.NetworkType != "" || len(net.ServiceNetwork) > 0 ||
		len(net.MachineNetwork) > 0 || len(net.ClusterNetwork) > 0 {
		fmt.Fprintln(bw, "\nNetworking:")
		if net.NetworkType != "" {
			fmt.Fprintf(bw, "  Type:          %s\n", net.NetworkType)
		}
		for _, mn := range net.MachineNetwork {
			fmt.Fprintf(bw, "  Machine CIDR:  %s\n", mn.CIDR)
		}
		for _, sn := range net.ServiceNetwork {
			fmt.Fprintf(bw, "  Service CIDR:  %s\n", sn)
		}
		for _, cn := range net.ClusterNetwork {
			fmt.Fprintf(bw, "  Cluster CIDR:  %s (/%d)\n", cn.CIDR, cn.HostPrefix)
		}
	}

	if m := c.Spec.Maintenance; m != nil {
		fmt.Fprintln(bw, "\nUpgrade Policy:")
		if m.Window != nil {
			fmt.Fprintf(bw, "  Start Time:    %s\n", m.Window.StartTime.UTC().Format(time.RFC3339))
			fmt.Fprintf(bw, "  Duration:      %d seconds\n", m.Window.DurationSeconds)
			fmt.Fprintf(bw, "  Frequency:     %s\n", m.Window.Recurrence.Frequency)
			fmt.Fprintf(bw, "  Days:          %s\n", strings.Join(m.Window.Recurrence.DaysOfWeek, ", "))
		} else {
			fmt.Fprintln(bw, "  Window:        none")
		}
		if m.Exclusions != nil && len(*m.Exclusions) > 0 {
			fmt.Fprintln(bw, "  Exclusions:")
			for _, ex := range *m.Exclusions {
				fmt.Fprintf(bw, "    %s: %s to %s\n", ex.Name, ex.StartTime.UTC().Format(time.RFC3339), ex.EndTime.UTC().Format(time.RFC3339))
			}
		}
	}

	if len(c.Status.Conditions) > 0 {
		fmt.Fprintln(bw, "\nConditions:")
		t := output.NewTable(bw, "TYPE", "STATUS", "REASON", "MESSAGE", "LAST TRANSITION")
		for _, cond := range c.Status.Conditions {
			msg := cond.Message
			if len(msg) > 80 {
				msg = msg[:80] + "..."
			}
			t.AddRow(
				cond.Type,
				string(cond.Status),
				cond.Reason,
				msg,
				cond.LastTransitionTime.UTC().Format(time.RFC3339),
			)
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}

	return bw.Flush()
}

func clusterStatus(c *gcpv1.Cluster) string {
	phase, _ := deriveClusterStatus(c)
	return phase
}

func clusterStatusDetail(c *gcpv1.Cluster) string {
	phase, detail := deriveClusterStatus(c)
	if detail == "" {
		return phase
	}
	return fmt.Sprintf("%s (%s)", phase, detail)
}

func deriveClusterStatus(c *gcpv1.Cluster) (phase, detail string) {
	return deriveStatusFromConditions(
		c.Status.Conditions,
		c.DeletionTimestamp,
		c.Generation,
		"HostedClusterAvailable",
		"", // clusters don't have a separate health condition
	)
}

// deriveStatusFromConditions derives a Ready/Progressing/Degraded status from
// an availability condition and, optionally, a health condition.
//
// Gecko's conditions have no "sticky"/last-known-good signal, so an
// availability condition that is merely False can't be distinguished from a
// resource that's still provisioning normally (e.g. HostedClusterAvailable
// is False for the entire control-plane bootstrap window, not just on
// failure) - that state is reported as "Progressing", not "Degraded".
//
// Once availability is confirmed True, a health condition (if the resource
// has one) gives an independent second signal: a health condition that
// definitively reports False after availability succeeded is a real
// regression rather than routine bring-up, so that combination is reported
// as "Degraded".
func deriveStatusFromConditions(
	conditions []metav1.Condition,
	deletionTimestamp *metav1.Time,
	generation int64,
	availableConditionType string,
	healthyConditionType string, // empty string if no health condition
) (phase, detail string) {
	if deletionTimestamp != nil {
		return "Deleting", ""
	}
	if len(conditions) == 0 {
		return "Pending", ""
	}

	available := meta.FindStatusCondition(conditions, availableConditionType)
	if available == nil || available.Status != metav1.ConditionTrue {
		return "Progressing", conditionSummary(available, generation)
	}

	// Available is confirmed True.
	if healthyConditionType == "" {
		return "Ready", ""
	}
	healthy := meta.FindStatusCondition(conditions, healthyConditionType)
	switch {
	case healthy != nil && healthy.Status == metav1.ConditionTrue:
		return "Ready", ""
	case healthy != nil && healthy.Status == metav1.ConditionFalse:
		return "Degraded", conditionSummary(healthy, generation)
	default:
		// Health hasn't reported yet or is still Unknown - no confirmed
		// problem, so keep showing Progressing.
		return "Progressing", conditionSummary(available, generation)
	}
}

func conditionSummary(cond *metav1.Condition, generation int64) string {
	if cond == nil {
		return ""
	}
	if cond.ObservedGeneration < generation && cond.ObservedGeneration > 0 {
		return fmt.Sprintf("controller reconciling generation %d", generation)
	}

	if cond.Message != "" {
		return truncateString(cond.Message, 60)
	}
	if cond.Reason == "" || cond.Reason == cond.Type {
		// Some conditions (e.g. gecko's HostedClusterAvailable) set Reason to
		// the literal condition Type with no Message, which adds no
		// information beyond the phase itself - omit rather than showing a
		// redundant "(HostedClusterAvailable)".
		return ""
	}
	return cond.Reason
}

func truncateString(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func releaseVersion(c *gcpv1.Cluster) string {
	if c.Spec.Release.Version != "" {
		return c.Spec.Release.Version
	}
	return "<none>"
}
