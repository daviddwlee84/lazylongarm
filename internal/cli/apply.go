package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/connection"
	"github.com/daviddwlee84/lazylongarm/internal/plan"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

// errBack: the user chose Back on the Review screen.
var errBack = errors.New("back to the form")

type applyOptions struct {
	yes, dryRun, forceResume bool
}

func (a *applyOptions) bind(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&a.yes, "yes", "y", false, "apply without the interactive Review (required when not in a terminal)")
	cmd.Flags().BoolVar(&a.dryRun, "dry-run", false, "print the plan (with --json: as JSON) and change nothing")
	cmd.Flags().BoolVar(&a.forceResume, "force-resume", false, "resume although a previous run left a step with an unknown outcome (after inspecting it)")
}

// review shows the plan and returns nil (apply), errBack or a cancel error.
func (o *options) review(cmd *cobra.Command, p *plan.Plan, ao applyOptions) error {
	if ao.yes {
		return nil
	}
	if !o.interactive(cmd) {
		return usage("applying %s needs confirmation: inspect it with --dry-run, then pass --yes", p.Kind)
	}
	var body bytes.Buffer
	p.Render(&body)
	ok, err := wizard.ConfirmBack(cmd.Context(), "Review · "+p.Title, body.String(), cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return err
	}
	if !ok {
		return errBack
	}
	return nil
}

// apply runs a reviewed plan with progress output, terminal handoffs, a
// journal, soft stop on the first Ctrl+C and an interactive retry loop.
func (o *options) apply(cmd *cobra.Command, p *plan.Plan, env *plan.Env, ao applyOptions) ([]plan.Result, error) {
	interactive := o.interactive(cmd)
	errOut := cmd.ErrOrStderr()
	if interactive {
		env.Interactive = func(ctx context.Context, req connection.Request) error {
			c, err := connection.InteractiveCommand(ctx, req)
			if err != nil {
				return err
			}
			c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), errOut
			fmt.Fprintln(errOut, "  ── terminal handed over ──")
			err = o.deps.Exec(c)
			fmt.Fprintln(errOut, "  ── back in lazylongarm ──")
			return err
		}
		env.Authenticate = func(ctx context.Context, host string) error {
			c, err := connection.AuthenticateCommand(ctx, host)
			if err != nil {
				return err
			}
			c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), errOut
			return o.deps.Exec(c)
		}
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	journal := plan.OpenJournal(stateDir, p.Kind, p.Key)
	journal.ForceResume = ao.forceResume
	obs := newTextObserver(errOut, len(p.Steps), o.colorEnabled(errOut))

	// First Ctrl+C: stop after the current step. Second: abort it.
	parent := cmd.Context()
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	go func() {
		count := 0
		for {
			select {
			case <-sigs:
				count++
				if count == 1 {
					close(stop)
					fmt.Fprintln(errOut, "\n  stopping after the current step (Ctrl+C again to abort it)")
				} else {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	runner := &plan.Runner{Observer: obs, Journal: journal, Stop: stop}
	fmt.Fprintf(errOut, "%s  (journal: %s)\n", obs.bold(p.Title), journal.Path())
	for {
		results, err := runner.Run(ctx, p, env, p.Digest())
		if err == nil {
			obs.summary(results)
			return results, nil
		}
		var step *plan.StepError
		if !interactive || !errors.As(err, &step) || ctx.Err() != nil {
			return results, err
		}
		title := fmt.Sprintf("Step %d failed: %s — %s", step.Index+1, step.Result.Title, truncate(step.Result.Message, 300))
		if step.Result.Fix != "" {
			title += " (fix: " + step.Result.Fix + ")"
		}
		choice, cerr := wizard.Choose(parent, title, []wizard.Choice{
			{Value: "retry", Label: "Retry (finished steps are re-checked and skipped)"},
			{Value: "quit", Label: "Quit (the journal keeps the state; rerun the same command to resume)"},
		}, cmd.InOrStdin(), cmd.OutOrStdout())
		if cerr != nil || choice != "retry" {
			return results, err
		}
		if step.Result.Status == plan.Unknown {
			journal.ForceResume = true // the user chose to retry after seeing it
		}
		fmt.Fprintln(errOut)
	}
}

func (o *options) colorEnabled(w io.Writer) bool {
	switch o.color {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && terminals(os.Stdin, f)
}

type textObserver struct {
	w     io.Writer
	total int
	color bool
	start map[int]time.Time
}

func newTextObserver(w io.Writer, total int, color bool) *textObserver {
	return &textObserver{w: w, total: total, color: color, start: map[int]time.Time{}}
}

func (t *textObserver) style(s string, c string) string {
	if !t.color {
		return s
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(s)
}
func (t *textObserver) bold(s string) string {
	if !t.color {
		return s
	}
	return lipgloss.NewStyle().Bold(true).Render(s)
}

func (t *textObserver) Started(i int, s *plan.Step) {
	t.start[i] = time.Now()
	fmt.Fprintf(t.w, "\n%s %s\n", t.style(fmt.Sprintf("[%d/%d]", i+1, t.total), "12"), t.bold(s.Title))
}

func (t *textObserver) Line(i int, line string) { fmt.Fprintln(t.w, t.style(line, "8")) }

func (t *textObserver) Finished(i int, r plan.Result) {
	d := time.Since(t.start[i]).Round(100 * time.Millisecond)
	switch r.Status {
	case plan.Done:
		fmt.Fprintf(t.w, "  %s %s\n", t.style("✓ done", "10"), t.style(d.String(), "8"))
	case plan.Skipped:
		fmt.Fprintf(t.w, "  %s\n", t.style("↷ already in place", "14"))
	case plan.Verified:
		fmt.Fprintf(t.w, "  %s %s\n", t.style("✓ verified", "10"), t.style(d.String(), "8"))
	case plan.Unknown:
		fmt.Fprintf(t.w, "  %s %s\n", t.style("? outcome unknown:", "11"), r.Message)
	case plan.NeedsAuth, plan.NeedsSudo:
		fmt.Fprintf(t.w, "  %s %s\n", t.style("! needs you:", "11"), r.Message)
	default:
		fmt.Fprintf(t.w, "  %s %s\n", t.style("✗ failed:", "9"), r.Message)
	}
	if r.Fix != "" {
		fmt.Fprintf(t.w, "    fix: %s\n", r.Fix)
	}
}

func (t *textObserver) summary(results []plan.Result) {
	done, skipped, verified := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case plan.Done:
			done++
		case plan.Skipped:
			skipped++
		case plan.Verified:
			verified++
		}
	}
	fmt.Fprintf(t.w, "\n%s %d applied, %d already in place, %d verified\n", t.style("✓ all steps finished:", "10"), done, skipped, verified)
}

// printPlan renders a dry run.
func (o *options) printPlan(cmd *cobra.Command, p *plan.Plan) error {
	if o.json {
		return writeJSON(cmd.OutOrStdout(), p.JSON())
	}
	p.Render(cmd.OutOrStdout())
	return nil
}

// probeWithAuth probes host and, in a terminal, offers an SSH login handoff
// once when authentication is required.
func (o *options) withAuth(cmd *cobra.Command, host string, fn func() error) error {
	err := fn()
	if err == nil || !connection.IsAuthRequired(err) || !o.interactive(cmd) {
		return err
	}
	ok, cerr := wizard.Confirm(cmd.Context(), "SSH login needed", fmt.Sprintf("ssh needs you to log in to %s (password, passphrase or host-key approval).\n\nHand the terminal to ssh now? lazylongarm never sees what you type.", host), cmd.InOrStdin(), cmd.OutOrStdout())
	if cerr != nil || !ok {
		return err
	}
	c, aerr := connection.AuthenticateCommand(cmd.Context(), host)
	if aerr != nil {
		return aerr
	}
	c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if aerr := o.deps.Exec(c); aerr != nil {
		return fmt.Errorf("ssh login to %s failed: %w", host, aerr)
	}
	return fn()
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
