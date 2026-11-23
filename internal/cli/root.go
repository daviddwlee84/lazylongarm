// Package cli is lazylongarm's command tree. Commands, wizards and the
// dashboard share the flows/plan services; stdout carries command data and
// diagnostics go to stderr.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/companion"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// Version is set with -ldflags "-X github.com/daviddwlee84/lazylongarm/internal/cli.Version=v0.1.0".
var Version = "dev"

// UsageError is a syntax/completeness error (exit 2).
type UsageError struct{ Message string }

func (e *UsageError) Error() string { return e.Message }
func usage(format string, args ...any) error {
	return &UsageError{fmt.Sprintf(format, args...)}
}

// ExitCode maps errors to the documented exit contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, wizard.ErrCanceled) || errors.Is(err, plan.ErrStopped) {
		return 130
	}
	var u *UsageError
	if errors.As(err, &u) || strings.HasPrefix(err.Error(), "unknown command ") || strings.HasPrefix(err.Error(), "unknown flag") {
		return 2
	}
	return 1
}

// Dependencies are test seams.
type Dependencies struct {
	Terminal func(io.Reader, io.Writer) bool
	RunTUI   func(ctx context.Context, opts *options) error
	Exec     func(*exec.Cmd) error // runs a command attached to the terminal
	// Companion finds and queries hc/herdr on this machine.
	Companion companion.Tools
	// Longarm runs the longarm CLI with LONGARM_CONFIG_DIR=configDir
	// ("" = Longarm's default) and returns its stdout.
	Longarm func(ctx context.Context, configDir string, args ...string) ([]byte, error)
}

type options struct {
	configPath string
	json       bool
	color      string
	deps       Dependencies
	in         io.Reader
	out, err   io.Writer
	cfg        *config.Config
}

// Execute is the process boundary.
func Execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return execute(ctx, args, in, out, errOut, Dependencies{})
}

func execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, deps Dependencies) int {
	root, o := New(deps)
	o.in, o.out, o.err = in, out, errOut
	root.SetArgs(append([]string{}, args...))
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	jsonMode := jsonRequested(root, args)
	err := root.ExecuteContext(ctx)
	_ = connection.CloseAuthentications()
	if err != nil {
		if jsonMode {
			enc := json.NewEncoder(errOut)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(map[string]any{"error": describeError(err)})
		} else {
			_, _ = fmt.Fprintln(errOut, "lazylongarm:", connection.Sanitize(err.Error()))
			var fix *plan.FixError
			if errors.As(err, &fix) && fix.Fix != "" {
				_, _ = fmt.Fprintln(errOut, "  fix:", fix.Fix)
			}
		}
		return ExitCode(err)
	}
	return 0
}

func describeError(err error) map[string]string {
	d := map[string]string{"code": "runtime", "message": connection.Sanitize(err.Error())}
	var u *UsageError
	var step *plan.StepError
	var sudo *plan.NeedsSudoError
	var fix *plan.FixError
	switch {
	case errors.As(err, &u), strings.HasPrefix(err.Error(), "unknown command "), strings.HasPrefix(err.Error(), "unknown flag"):
		d["code"] = "usage"
	case connection.IsAuthRequired(err):
		d["code"] = "ssh-auth-required"
	case errors.As(err, &sudo):
		d["code"] = "sudo-password-required"
	case errors.Is(err, config.ErrConflict):
		d["code"] = "config-conflict"
	case errors.Is(err, context.Canceled), errors.Is(err, wizard.ErrCanceled), errors.Is(err, plan.ErrStopped):
		d["code"] = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		d["code"] = "timeout"
	case errors.As(err, &step):
		d["code"] = "step-" + string(step.Result.Status)
		d["step"] = step.Result.ID
	}
	if errors.As(err, &fix) && fix.Fix != "" {
		d["fix"] = fix.Fix
	}
	return d
}

// New builds the command tree.
func New(deps Dependencies) (*cobra.Command, *options) {
	if deps.Terminal == nil {
		deps.Terminal = terminals
	}
	if deps.Exec == nil {
		deps.Exec = func(c *exec.Cmd) error { return c.Run() }
	}
	if deps.Companion.Run == nil {
		deps.Companion = companion.System()
	}
	if deps.Longarm == nil {
		deps.Longarm = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
			return (&longarm.Client{ConfigDir: configDir}).Run(ctx, 2*time.Minute, nil, args...)
		}
	}
	if deps.RunTUI == nil {
		deps.RunTUI = runDashboard
	}
	o := &options{deps: deps, in: os.Stdin, out: os.Stdout, err: os.Stderr}
	root := &cobra.Command{
		Use:   "lazylongarm",
		Short: "A keyboard-first console and setup wizard for Longarm fleets",
		Long: `lazylongarm sets up and operates a Longarm fleet (https://github.com/lubobill1990/longarm):
install a longarm-hub on an SSH-reachable machine, enroll this machine, bootstrap
workers over SSH, and watch workers and jobs in a lazygit-style dashboard.

Run without a subcommand to open the dashboard. Agents can read the bundled
operating guide with --skill.`,
		Version:       versionString(),
		SilenceErrors: true, SilenceUsage: true,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if skill, _ := cmd.Flags().GetBool("skill"); skill {
				return printSkill(cmd.OutOrStdout(), "")
			}
			if o.json {
				return usage("--json needs a data command, e.g. `lazylongarm status --json`")
			}
			if !o.deps.Terminal(cmd.InOrStdin(), cmd.OutOrStdout()) {
				return cmd.Help()
			}
			if err := o.load(); err != nil {
				return err
			}
			return o.deps.RunTUI(cmd.Context(), o)
		},
	}
	// Forms follow --color (and NO_COLOR in auto). No subcommand defines its
	// own persistent pre-run, so this one runs for every command.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cmd.SetContext(wizard.WithColor(cmd.Context(), o.colorEnabled(cmd.OutOrStdout())))
		return nil
	}
	root.SetVersionTemplate("lazylongarm {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&o.configPath, "config", "", "config file (env LAZYLONGARM_CONFIG; default $XDG_CONFIG_HOME/lazylongarm/config.toml)")
	pf.BoolVar(&o.json, "json", false, "machine-readable output (never prompts)")
	pf.StringVar(&o.color, "color", "auto", "auto | always | never (NO_COLOR is honored in auto)")
	root.Flags().Bool("skill", false, "print the bundled agent operating guide")
	root.AddCommand(hubCommand(o), workerCommand(o), peerCommand(o), userCommand(o), statusCommand(o), settingsCommand(o), companionCommand(o), skillCommand(o), versionCommand(o), upgradeCommand(o))
	o.wireCompletion(root)
	return root, o
}

func (o *options) load() error {
	if o.cfg != nil {
		return nil
	}
	path, explicit, err := config.ResolvePath(o.configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path, explicit)
	if err != nil {
		return err
	}
	o.cfg = cfg
	warnUnknownKeys(o.err, cfg)
	return nil
}

// warnUnknownKeys tells the user (on stderr, so --json output stays clean)
// that the config has keys this build does not know. They usually come from
// a newer lazylongarm sharing the file; a hand-edited typo shows up here too.
func warnUnknownKeys(w io.Writer, cfg *config.Config) {
	keys := cfg.UnknownKeys()
	if len(keys) == 0 || w == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "! %s: settings unknown to lazylongarm %s: %s\n", cfg.Path(), versionString(), strings.Join(keys, ", "))
	_, _ = fmt.Fprintln(w, "  probably from a newer lazylongarm (run `lazylongarm upgrade`), or a typo; they are kept when lazylongarm saves")
}

func (o *options) interactive(cmd *cobra.Command) bool {
	return !o.json && o.deps.Terminal(cmd.InOrStdin(), cmd.OutOrStdout())
}

func terminals(in io.Reader, out io.Writer) bool {
	i, ok := in.(*os.File)
	if !ok {
		return false
	}
	f, ok := out.(*os.File)
	return ok && term.IsTerminal(int(i.Fd())) && term.IsTerminal(int(f.Fd()))
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		if cmd.HasAvailableSubCommands() {
			return usage("unknown command %q for %q; see --help", args[0], cmd.CommandPath())
		}
		return usage("%s takes no arguments; see --help", cmd.CommandPath())
	}
	return nil
}

func groupHelp(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usage("unknown command %q for %q; see --help", args[0], cmd.CommandPath())
	}
	return cmd.Help()
}

func versionString() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func versionCommand(o *options) *cobra.Command {
	return &cobra.Command{
		Use: "version", Short: "Print the lazylongarm version", Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"version": versionString()})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "lazylongarm", versionString())
			return err
		},
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonRequested inspects argv before execution so syntax errors are JSON too
// (adapted from lazyclash internal/cli/execute.go).
func jsonRequested(root *cobra.Command, args []string) bool {
	valueFlags := map[string]bool{}
	var visit func(*cobra.Command)
	visit = func(cmd *cobra.Command) {
		for _, flags := range []*pflag.FlagSet{cmd.LocalNonPersistentFlags(), cmd.PersistentFlags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				if flag.NoOptDefVal == "" {
					valueFlags["--"+flag.Name] = true
					if flag.Shorthand != "" {
						valueFlags["-"+flag.Shorthand] = true
					}
				}
			})
		}
		for _, child := range cmd.Commands() {
			visit(child)
		}
	}
	visit(root)
	requested := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name == "--json" {
			if !hasValue {
				requested = true
			} else if parsed, err := strconv.ParseBool(value); err == nil {
				requested = parsed
			}
			continue
		}
		if !hasValue && valueFlags[name] {
			i++
		}
	}
	return requested
}
