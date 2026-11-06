package cluster

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/output"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type upgradePolicyOptions struct {
	startTime       string
	durationSeconds int32
	frequency       string
	days            []string
	exclusions      []string
	outputFmt       string
}

// newSetUpgradePolicyCmd configures the upgrade policy on an existing cluster by
// patching Cluster.spec.maintenance in-place. The command is idempotent: running
// it multiple times with the same arguments produces the same cluster state.
func newSetUpgradePolicyCmd() *cobra.Command {
	opts := &upgradePolicyOptions{}
	var clearExclusions bool
	cmd := &cobra.Command{
		Use:   "set-upgrade-policy <cluster-name>",
		Short: "Set the control-plane upgrade policy on an existing cluster",
		Long: "Configure the upgrade policy on an existing cluster by patching spec.maintenance directly. " +
			"The command is idempotent: running it multiple times with the same arguments produces the same result. " +
			"Existing maintenance exclusions are preserved unless --exclusion replaces them or --clear-exclusions removes them.",
		Args: clusterNameArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			maintenance, err := opts.build()
			if err != nil {
				return err
			}

			// Build a merge-patch that only touches spec.maintenance.
			// Omitted merge-patch fields inside maintenance preserve their current
			// values on the server. Use an explicit empty slice to clear exclusions
			// despite the API's omitempty tag.
			maintenancePatch := map[string]any{
				"window": maintenance.Window,
			}
			if clearExclusions {
				maintenancePatch["exclusions"] = []gcpv1.ControlPlaneMaintenanceExclusion{}
			} else if cmd.Flags().Changed("exclusion") {
				maintenancePatch["exclusions"] = maintenance.Exclusions
			}
			patchData, err := json.Marshal(map[string]any{
				"spec": map[string]any{"maintenance": maintenancePatch},
			})
			if err != nil {
				return fmt.Errorf("encoding upgrade policy: %w", err)
			}

			client := clientFromCmd(cmd)
			updated, err := client.Clusters().Patch(cmd.Context(), client.Namespace(), args[0], patchData)
			if err != nil {
				return fmt.Errorf("setting upgrade policy: %w", err)
			}
			return printUpgradePolicy(cmd.OutOrStdout(), updated, opts.outputFmt)
		},
	}
	addUpgradePolicyFlags(cmd, opts)
	cmd.Flags().BoolVar(&clearExclusions, "clear-exclusions", false, "Remove all maintenance exclusions")
	cmd.MarkFlagsMutuallyExclusive("exclusion", "clear-exclusions")
	return cmd
}

// newClearUpgradePolicyCmd removes the upgrade policy (spec.maintenance) from an
// existing cluster, allowing the platform to start automatic upgrades at any time.
// The command is idempotent: running it on a cluster that has no policy is a no-op.
// --confirm is required because removing the maintenance window may cause an
// automatic upgrade to begin immediately.
func newClearUpgradePolicyCmd() *cobra.Command {
	var outputFmt string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "clear-upgrade-policy <cluster-name>",
		Short: "Remove the control-plane upgrade policy from an existing cluster",
		Long: "Remove the upgrade policy (spec.maintenance) from an existing cluster. " +
			"After clearing, the platform may start automatic upgrades at any time. " +
			"The command is idempotent: running it on a cluster that already has no policy succeeds without error. " +
			"Requires --confirm.",
		Args: clusterNameArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("--confirm is required to clear an upgrade policy")
			}

			// A JSON merge patch with an explicit null removes spec.maintenance even
			// though ClusterSpec.Maintenance is tagged omitempty: the raw bytes reach
			// the API server without going through typed-object serialisation, so
			// omitempty cannot suppress the null.
			patchData := []byte(`{"spec":{"maintenance":null}}`)

			client := clientFromCmd(cmd)
			updated, err := client.Clusters().Patch(cmd.Context(), client.Namespace(), args[0], patchData)
			if err != nil {
				return fmt.Errorf("clearing upgrade policy: %w", err)
			}
			return printUpgradePolicy(cmd.OutOrStdout(), updated, outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm removal of the upgrade policy (required)")
	return cmd
}

// clusterNameArgs requires one cluster name and includes usage when it is missing.
func clusterNameArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("cluster name is required\n\nUsage: %s", cmd.UseLine())
	}
	return cobra.ExactArgs(1)(cmd, args)
}

// addUpgradePolicyFlags registers the schedule, exclusion, and output flags.
func addUpgradePolicyFlags(cmd *cobra.Command, opts *upgradePolicyOptions) {
	cmd.Flags().StringVar(&opts.startTime, "start-time", "", "First maintenance window start time in RFC 3339 UTC format (required)")
	cmd.Flags().Int32Var(&opts.durationSeconds, "duration-seconds", 0, "Maintenance window duration in seconds; must be between 60 and 86400 (required)")
	cmd.Flags().StringVar(&opts.frequency, "frequency", "weekly", "Maintenance window frequency (weekly)")
	cmd.Flags().StringSliceVar(&opts.days, "day", nil, "Maintenance window day as a lowercase full weekday name; repeat for multiple days (required)")
	cmd.Flags().StringArrayVar(&opts.exclusions, "exclusion", nil, "Exclusion as name,start-time,end-time (RFC 3339); repeat for multiple exclusions")
	cmd.Flags().StringVarP(&opts.outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	for _, name := range []string{"start-time", "duration-seconds", "day"} {
		if err := cmd.MarkFlagRequired(name); err != nil {
			// All flags are registered above; a missing flag is a programming error.
			panic(fmt.Errorf("marking upgrade policy flag %q required: %w", name, err))
		}
	}
}

// build validates schedule inputs and constructs a ClusterMaintenanceSpec with UTC timestamps.
func (o *upgradePolicyOptions) build() (*gcpv1.ClusterMaintenanceSpec, error) {
	startTime, err := time.Parse(time.RFC3339, o.startTime)
	if err != nil {
		return nil, fmt.Errorf("--start-time must be an RFC 3339 timestamp: %w", err)
	}
	if o.durationSeconds < 60 || o.durationSeconds > 86400 {
		return nil, fmt.Errorf("--duration-seconds must be between 60 and 86400")
	}
	if len(o.days) == 0 {
		return nil, fmt.Errorf("at least one --day is required")
	}
	if o.frequency != "weekly" {
		return nil, fmt.Errorf("--frequency must be weekly")
	}
	seenDays := make(map[string]struct{}, len(o.days))
	for _, day := range o.days {
		switch day {
		case "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday":
		default:
			return nil, fmt.Errorf("--day contains invalid day %q; use lowercase full weekday names", day)
		}
		if _, duplicate := seenDays[day]; duplicate {
			return nil, fmt.Errorf("--day contains duplicate day %q", day)
		}
		seenDays[day] = struct{}{}
	}

	exclusions, err := parseExclusions(o.exclusions)
	if err != nil {
		return nil, err
	}

	return &gcpv1.ClusterMaintenanceSpec{
		Window: &gcpv1.ControlPlaneMaintenanceWindow{
			StartTime:       metav1.NewTime(startTime.UTC()),
			DurationSeconds: o.durationSeconds,
			Recurrence: gcpv1.ControlPlaneMaintenanceRecurrence{
				Frequency:  o.frequency,
				DaysOfWeek: o.days,
			},
		},
		Exclusions: &exclusions,
	}, nil
}

// parseExclusions decodes repeated name,start-time,end-time values and normalizes times to UTC.
func parseExclusions(values []string) ([]gcpv1.ControlPlaneMaintenanceExclusion, error) {
	exclusions := make([]gcpv1.ControlPlaneMaintenanceExclusion, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("--exclusion must be name,start-time,end-time")
		}
		startTime, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			return nil, fmt.Errorf("parsing exclusion start time: %w", err)
		}
		endTime, err := time.Parse(time.RFC3339, parts[2])
		if err != nil {
			return nil, fmt.Errorf("parsing exclusion end time: %w", err)
		}
		exclusions = append(exclusions, gcpv1.ControlPlaneMaintenanceExclusion{
			Name:      parts[0],
			StartTime: metav1.NewTime(startTime.UTC()),
			EndTime:   metav1.NewTime(endTime.UTC()),
		})
	}
	return exclusions, nil
}

// printUpgradePolicy writes the maintenance policy section of a cluster in text, JSON, or YAML.
func printUpgradePolicy(w io.Writer, cluster *gcpv1.Cluster, format string) error {
	switch output.ParseFormat(format) {
	case output.FormatJSON:
		return output.PrintJSON(w, cluster)
	case output.FormatYAML:
		return output.PrintResourceYAML(w, cluster)
	}

	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "Cluster: %s\n", cluster.Name)
	m := cluster.Spec.Maintenance
	if m == nil || m.Window == nil {
		fmt.Fprintln(bw, "Upgrade policy: none (upgrades may start at any time)")
		return bw.Flush()
	}
	fmt.Fprintf(bw, "Start Time: %s\n", m.Window.StartTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(bw, "Duration: %d seconds\n", m.Window.DurationSeconds)
	fmt.Fprintf(bw, "Frequency: %s\n", m.Window.Recurrence.Frequency)
	fmt.Fprintf(bw, "Days: %s\n", strings.Join(m.Window.Recurrence.DaysOfWeek, ", "))
	if m.Exclusions != nil && len(*m.Exclusions) > 0 {
		fmt.Fprintln(bw, "Exclusions:")
		for _, ex := range *m.Exclusions {
			fmt.Fprintf(bw, "  %s: %s to %s\n", ex.Name, ex.StartTime.UTC().Format(time.RFC3339), ex.EndTime.UTC().Format(time.RFC3339))
		}
	}
	return bw.Flush()
}
