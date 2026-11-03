package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/httpdebug"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

func TestVerbosityHelp(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-v, --v int", "0-5: no HTTP tracing", "6: metadata", "7: request headers", "8: bodies", "9: curl/timing", "10: untruncated supported bodies", "stderr", "GCPHCPCTL_VERBOSITY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "--verbose") {
		t.Error("help retains removed --verbose flag")
	}
	if strings.Contains(out.String(), "metadata only") {
		t.Error("help still claims all levels are metadata only")
	}
}

func TestVerbosityConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		env  string
		args []string
		want int
	}{
		{name: "default"},
		{name: "file six", file: "verbosity: 6\n", want: 6},
		{name: "file zero", file: "verbosity: 0\n"},
		{name: "env six", file: "verbosity: 0\n", env: "6", want: 6},
		{name: "env zero overrides file", file: "verbosity: 6\n", env: "0"},
		{name: "flag six overrides env", env: "0", args: []string{"--v=6"}, want: 6},
		{name: "flag zero overrides env and file", file: "verbosity: 6\n", env: "6", args: []string{"--v=0"}},
		{name: "flag zero overrides file", file: "verbosity: 6\n", args: []string{"--v=0"}},
		{name: "flag zero overrides invalid env", env: "secret-not-an-integer", args: []string{"--v=0"}},
		{name: "flag six overrides invalid env", env: "secret-not-an-integer", args: []string{"--v=6"}, want: 6},
		{name: "flag overrides negative env", env: "-1", args: []string{"--v=6"}, want: 6},
		{name: "short equals", args: []string{"-v=6"}, want: 6},
		{name: "short attached", args: []string{"-v6"}, want: 6},
		{name: "short separate", args: []string{"-v", "6"}, want: 6},
		{name: "long separate", args: []string{"--v", "6"}, want: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GCPHCPCTL_PROJECT", "")
			t.Setenv("GCPHCPCTL_REGION", "")
			t.Setenv("GCPHCPCTL_VERBOSITY", tc.env)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("project: file-project\nregion: file-region\noutput: json\n"+tc.file), 0600); err != nil {
				t.Fatal(err)
			}
			root := newRootCmd()
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			ran := false
			root.AddCommand(&cobra.Command{
				Use: "probe",
				RunE: func(cmd *cobra.Command, _ []string) error {
					ran = true
					got, err := cmd.Flags().GetInt("v")
					if err != nil || got != tc.want {
						t.Errorf("verbosity = %v, %v; want %v", got, err, tc.want)
					}
					for name, want := range map[string]string{"project": "file-project", "region": "file-region", "output": "json"} {
						got, err := cmd.Flags().GetString(name)
						if err != nil || got != want {
							t.Errorf("%s = %q, %v; want %q", name, got, err, want)
						}
					}
					return nil
				},
			})
			root.SetArgs(append([]string{"probe", "--config", path}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !ran {
				t.Fatal("probe command did not run")
			}
			if out.Len() != 0 || stderr.Len() != 0 {
				t.Errorf("unexpected output: stdout=%q stderr=%q", out.String(), stderr.String())
			}
		})
	}
}

func TestVerbosityInvalidEnvironment(t *testing.T) {
	t.Setenv("GCPHCPCTL_VERBOSITY", "secret-not-an-integer")
	root := newRootCmd()
	root.AddCommand(&cobra.Command{Use: "probe", Run: func(*cobra.Command, []string) {
		t.Error("command ran with invalid verbosity value")
	}})
	root.SetArgs([]string{"probe", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--project", "project", "--region", "region"})
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	err := root.Execute()
	if err == nil || err.Error() != "GCPHCPCTL_VERBOSITY must be a nonnegative integer" {
		t.Fatalf("expected actionable integer error, got %v", err)
	}
	if strings.Contains(err.Error()+out.String()+stderr.String(), "secret-not-an-integer") {
		t.Fatal("invalid environment value leaked")
	}
}

func TestVerbosityRejectsInvalidInputBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	for _, tc := range []struct {
		name string
		file string
		env  string
		args []string
		want string
	}{
		{name: "negative long flag", args: []string{"--v=-1"}, want: "nonnegative integer"},
		{name: "negative short equals", args: []string{"-v=-1"}, want: "nonnegative integer"},
		{name: "negative short attached", args: []string{"-v-1"}, want: "nonnegative integer"},
		{name: "negative short separate", args: []string{"-v", "-1"}, want: "nonnegative integer"},
		{name: "boolean flag", args: []string{"--v=true"}, want: "invalid argument"},
		{name: "false flag", args: []string{"--v=false"}, want: "invalid argument"},
		{name: "fraction flag", args: []string{"--v=6.5"}, want: "invalid argument"},
		{name: "overflow flag", args: []string{"--v=999999999999999999999999999999"}, want: "invalid argument"},
		{name: "empty flag", args: []string{"--v="}, want: "invalid argument"},
		{name: "bare short flag", args: []string{"-v"}, want: "needs an argument"},
		{name: "bare long flag", args: []string{"--v"}, want: "needs an argument"},
		{name: "removed verbose flag", args: []string{"--verbose"}, want: "unknown flag"},
		{name: "removed verbose equals", args: []string{"--verbose=false"}, want: "unknown flag"},
		{name: "no verbosity alias", args: []string{"--verbosity=6"}, want: "unknown flag"},
		{name: "negative environment", env: "-1", want: "GCPHCPCTL_VERBOSITY must be a nonnegative integer"},
		{name: "boolean environment", env: "true", want: "GCPHCPCTL_VERBOSITY must be a nonnegative integer"},
		{name: "fraction environment", env: "6.5", want: "GCPHCPCTL_VERBOSITY must be a nonnegative integer"},
		{name: "overflow environment", env: "999999999999999999999999999999", want: "GCPHCPCTL_VERBOSITY must be a nonnegative integer"},
		{name: "negative file", file: "verbosity: -1\n", want: "nonnegative integer"},
		{name: "negative file with flag override", file: "verbosity: -1\n", args: []string{"--v=6"}, want: "nonnegative integer"},
		{name: "fraction file", file: "verbosity: 6.5\n", want: "nonnegative integer"},
		{name: "boolean file", file: "verbosity: true\n", want: "nonnegative integer"},
		{name: "overflow file", file: "verbosity: 999999999999999999999999999999\n", want: "parsing config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GCPHCPCTL_VERBOSITY", tc.env)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0600); err != nil {
				t.Fatal(err)
			}
			root := newRootCmd()
			var out, stderr bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.AddCommand(&cobra.Command{
				Use: "http-probe",
				PreRunE: func(*cobra.Command, []string) error {
					resp, err := client.Get(server.URL)
					if err != nil {
						return err
					}
					return resp.Body.Close()
				},
				Run: func(*cobra.Command, []string) {},
			})
			args := []string{"http-probe", "--config", path, "--project", "project", "--region", "region"}
			root.SetArgs(append(args, tc.args...))
			before := requests.Load()
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v; want %q", err, tc.want)
			}
			if got := requests.Load() - before; got != 0 {
				t.Errorf("invalid verbosity allowed %d HTTP requests", got)
			}
			if tc.env != "" && err != nil && strings.Contains(err.Error()+out.String()+stderr.String(), tc.env) {
				t.Error("invalid environment value leaked")
			}
		})
	}
}

// TestVerbosityHTTPInvocationIsolation checks that independent command invocations get
// fresh trace state and writers.
func TestVerbosityHTTPInvocationIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer request-credential-secret" || r.Header.Get("Cookie") != "session=request-cookie-secret" {
			t.Error("tracing changed request credentials on the wire")
		}
		w.Header().Set("X-Secret", "response-header-secret")
		fmt.Fprint(w, "response-body-secret")
	}))
	defer server.Close()
	defaultTransport := http.DefaultTransport
	client := &http.Client{Transport: httpdebug.Wrap(nil), Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	var inherited bytes.Buffer
	ctx := httpdebug.WithTrace(context.Background(), &inherited, 6)
	var previousTrace *bytes.Buffer
	var previousText string
	for _, tc := range []struct {
		name  string
		args  []string
		env   string
		file  string
		level int
	}{
		{name: "first traced invocation", args: []string{"-v=6"}, level: 6},
		{name: "fresh quiet invocation"},
		{name: "fresh trace writer", args: []string{"--v=7"}, level: 7},
		{name: "explicit zero disables inherited trace", args: []string{"--v=0"}, env: "6", file: "verbosity: 6\n"},
		{name: "level one", args: []string{"--v=1"}},
		{name: "level two", args: []string{"--v=2"}},
		{name: "level three", args: []string{"--v=3"}},
		{name: "level four", args: []string{"--v=4"}},
		{name: "level five", args: []string{"--v=5"}},
		{name: "level eight", args: []string{"--v=8"}, level: 8},
		{name: "level nine", args: []string{"--v=9"}, level: 9},
		{name: "level ten", args: []string{"--v=10"}, level: 10},
		{name: "file enables trace", file: "verbosity: 6\n", level: 6},
		{name: "file enables bodies", file: "verbosity: 8\n", level: 8},
		{name: "file zero disables inherited trace", file: "verbosity: 0\n"},
		{name: "environment enables trace", env: "6", level: 6},
		{name: "environment enables curl", env: "9", level: 9},
		{name: "environment zero overrides file", env: "0", file: "verbosity: 6\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GCPHCPCTL_VERBOSITY", tc.env)
			var out, stderr bytes.Buffer
			root := newRootCmd()
			root.SetContext(ctx)
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.AddCommand(&cobra.Command{
				Use: "http-probe",
				PreRunE: func(cmd *cobra.Command, _ []string) error {
					req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, server.URL+"/probe", nil)
					if err != nil {
						return err
					}
					req.Header.Set("Authorization", "Bearer request-credential-secret")
					req.Header.Set("Cookie", "session=request-cookie-secret")
					req.Header.Set("X-Probe", "unknown-header-secret")
					resp, err := client.Do(req)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					_, err = io.Copy(io.Discard, resp.Body)
					return err
				},
				RunE: func(cmd *cobra.Command, _ []string) error {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), `{"result":"ok"}`)
					return err
				},
			})
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"http-probe", "--config", path, "--project", "project", "--region", "region"}
			root.SetArgs(append(args, tc.args...))
			beforeKlog := make(map[klog.Level]bool)
			for _, level := range []klog.Level{0, 5, 6, 7, 8, 9, 10} {
				beforeKlog[level] = klog.V(level).Enabled()
			}
			executed, err := root.ExecuteC()
			if err != nil {
				t.Fatal(err)
			}
			for level, enabled := range beforeKlog {
				if klog.V(level).Enabled() != enabled {
					t.Errorf("CLI changed global klog verbosity at level %d", level)
				}
			}
			ctx = executed.Context()
			if out.String() != "{\"result\":\"ok\"}\n" {
				t.Errorf("stdout was changed by tracing: %q", out.String())
			}
			assertProgressiveTrace(t, stderr.String(), tc.level)
			if tc.level >= 6 {
				for _, want := range []string{"start", `method="GET"`, "http://REDACTED/probe", "complete", "status=200", "elapsed="} {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("stderr missing %q: %s", want, stderr.String())
					}
				}
			} else if stderr.Len() != 0 {
				t.Errorf("quiet invocation emitted HTTP trace: %s", stderr.String())
			}
			if strings.Contains(stderr.String(), "secret") {
				t.Errorf("trace leaked response data: %s", stderr.String())
			}
			if tc.level >= 8 && (!strings.Contains(stderr.String(), " response_body ") || !strings.Contains(stderr.String(), "omitted")) {
				t.Errorf("plain text response needs an explicit omission: %s", stderr.String())
			}
			if inherited.Len() != 0 {
				t.Errorf("inherited tracer received output: %s", inherited.String())
			}
			if previousTrace != nil && previousTrace.String() != previousText {
				t.Error("previous invocation's trace writer was reused")
			}
			previousTrace, previousText = &stderr, stderr.String()
		})
	}
	if http.DefaultTransport != defaultTransport {
		t.Error("invocation changed http.DefaultTransport")
	}
}

func assertProgressiveTrace(t *testing.T, trace string, level int) {
	t.Helper()
	if level < 6 {
		if trace != "" {
			t.Errorf("quiet command produced trace: %s", trace)
		}
		return
	}
	for event, threshold := range map[string]int{"start": 6, "complete": 6, "request_headers": 7, "response_headers": 8, "response_body": 8, "curl": 9, "timing": 9} {
		want := 0
		if level >= threshold {
			want = 1
		}
		if count := strings.Count(trace, " "+event+" "); count != want {
			t.Errorf("level %d: got %d %s events, want %d: %s", level, count, event, want, trace)
		}
	}
	if level < 8 && strings.Contains(trace, " request_body ") {
		t.Errorf("level %d exposed request body: %s", level, trace)
	}
	if level == 6 && strings.Count(trace, "\n") != 2 {
		t.Errorf("level 6 must have exactly one start and completion: %s", trace)
	}
}

func TestFreshInvocationPreservesOtherFlagPrecedence(t *testing.T) {
	t.Setenv("GCPHCPCTL_PROJECT", "env-project")
	t.Setenv("GCPHCPCTL_REGION", "env-region")
	t.Setenv("GCPHCPCTL_VERBOSITY", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("project: file-project\nregion: file-region\noutput: json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		project string
		region  string
		output  string
	}{
		{name: "flags", args: []string{"--project", "flag-project", "--region", "flag-region", "--output", "yaml"}, project: "flag-project", region: "flag-region", output: "yaml"},
		{name: "fresh environment", project: "env-project", region: "env-region", output: "json"},
		{name: "empty string flags use file", args: []string{"--project=", "--region=", "--output="}, project: "file-project", region: "file-region"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			root.AddCommand(&cobra.Command{Use: "probe", Run: func(cmd *cobra.Command, _ []string) {
				for name, want := range map[string]string{"project": tc.project, "region": tc.region, "output": tc.output} {
					got, err := cmd.Flags().GetString(name)
					if err != nil || got != want {
						t.Errorf("%s = %q, %v; want %q", name, got, err, want)
					}
				}
			}})
			root.SetArgs(append([]string{"probe", "--config", path}, tc.args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
