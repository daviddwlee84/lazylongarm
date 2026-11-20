package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/daviddwlee84/lazylongarm/internal/config"
	"github.com/daviddwlee84/lazylongarm/internal/tui"
)

func runDashboard(parent context.Context, o *options) error {
	// While Bubble Tea owns the terminal, Ctrl+C is a key, not a signal. A
	// SIGINT only arrives during a terminal handoff, and must cancel that
	// command alone, not the dashboard: so the dashboard ignores the
	// process-wide interrupt cancellation and only stops on SIGTERM.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	defer signal.Stop(term)
	go func() {
		select {
		case <-term:
			cancel()
		case <-ctx.Done():
		}
	}()
	refresh := 5 * time.Second
	if o.cfg.TUI.RefreshSeconds > 0 {
		refresh = time.Duration(o.cfg.TUI.RefreshSeconds) * time.Second
	}
	model := tui.New(tui.Options{
		Ctx:    ctx,
		Config: o.cfg,
		ReloadCfg: func() (*config.Config, error) {
			path, explicit, err := config.ResolvePath(o.configPath)
			if err != nil {
				return nil, err
			}
			return config.Load(path, explicit)
		},
		// Wizards run as the same Cobra commands, in-process, on the
		// terminal Bubble Tea releases for the handoff.
		RunCommand: func(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt) // Ctrl+C ends this command only
			defer stop()
			if o.configPath != "" {
				args = append([]string{"--config", o.configPath}, args...)
			}
			root, sub := New(o.deps)
			sub.in, sub.out, sub.err = in, out, errOut
			root.SetArgs(args)
			root.SetIn(in)
			root.SetOut(out)
			root.SetErr(errOut)
			return root.ExecuteContext(ctx)
		},
		Refresh: refresh,
		OpenURL: openBrowser,
		Color:   o.colorEnabled(o.out),
	})
	defer model.Close()
	_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(o.in), tea.WithOutput(o.out)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
