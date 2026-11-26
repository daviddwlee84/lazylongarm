package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/daviddwlee84/lazylongarm/internal/longarm"
	"github.com/daviddwlee84/lazylongarm/internal/wizard"
	"github.com/spf13/cobra"
)

func jobCommand(o *options) *cobra.Command {
	c := &cobra.Command{Use: "job", Short: "Submit jobs to workers (a form over longarm run / exec)", Args: cobra.ArbitraryArgs, RunE: groupHelp}
	c.AddCommand(jobRunCommand(o))
	return c
}

// jobRequest is one submission: a shell command line (Command) or an
// explicit argv, on a worker or "auto".
type jobRequest struct {
	Worker  string   // "" = auto (the hub places it)
	Exec    bool     // longarm exec (synchronous, no job) instead of longarm run
	Command string   // a shell command line, run with sh -c (powershell on Windows)
	Argv    []string // explicit argv; wins over Command
	Name    string   // job name (run only)
	Cwd     string
	Require []string      // KEY=VALUE labels the worker must have (run only)
	Timeout time.Duration // exec only; 0 = longarm's default
	Windows bool          // the worker runs Windows: wrap Command for PowerShell
}

// argv is what the worker runs.
func (r jobRequest) argv() []string {
	if len(r.Argv) > 0 {
		return r.Argv
	}
	if r.Windows {
		return []string{"powershell", "-NoProfile", "-Command", r.Command}
	}
	return []string{"sh", "-c", r.Command}
}

// longarmArgs are the arguments of the longarm command that submits r.
func (r jobRequest) longarmArgs() []string {
	verb := "run"
	if r.Exec {
		verb = "exec"
	}
	args := []string{verb}
	if r.Worker != "" && r.Worker != "auto" {
		args = append(args, "--on", r.Worker)
	}
	if r.Name != "" && !r.Exec {
		args = append(args, "--name", r.Name)
	}
	if r.Cwd != "" {
		args = append(args, "--cwd", r.Cwd)
	}
	if !r.Exec {
		for _, q := range r.Require {
			args = append(args, "--require", q)
		}
	}
	if r.Exec && r.Timeout > 0 {
		args = append(args, "--timeout", r.Timeout.String())
	}
	return append(append(args, "--"), r.argv()...)
}

// shellQuote quotes s for a POSIX shell, only when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// reviewText shows the exact command and what to check before it runs.
func (r jobRequest) reviewText(anyWindows bool) string {
	var b strings.Builder
	where := "the hub picks a worker (auto)"
	if r.Worker != "" && r.Worker != "auto" {
		where = "on " + r.Worker
	}
	mode := "a job (runs in the background; follow it in the Jobs tab or with longarm logs <id> -f)"
	if r.Exec {
		mode = "exec (runs now; output here, exit code mirrored, killed after the timeout)"
	}
	fmt.Fprintf(&b, "Submit %s, %s.\n\nThe exact command:\n\n  longarm %s\n", mode, where, shellJoin(r.longarmArgs()))
	if len(r.Argv) == 0 {
		shell := "sh -c"
		if r.Windows {
			shell = "powershell -Command"
		}
		fmt.Fprintf(&b, "\nThe worker runs your line with %s, exactly as typed:\n", shell)
		for _, l := range strings.Split(r.Command, "\n") {
			b.WriteString("  │ " + l + "\n")
		}
		if strings.Contains(r.Command, "'") && !r.Windows {
			b.WriteString("\n! Your line contains ' ; above it is quoted as '\\'' for this display only. The worker's shell receives your line unchanged.\n")
		}
		if (r.Worker == "" || r.Worker == "auto") && anyWindows && !r.Windows {
			b.WriteString("\n! auto may pick a Windows worker, where sh -c fails; add a requirement such as os=linux.\n")
		}
	}
	return b.String()
}

func jobRunCommand(o *options) *cobra.Command {
	var (
		req         jobRequest
		hubID       string
		interactive bool
	)
	c := &cobra.Command{
		Use:   "run [worker] [-- command [args...]]",
		Short: "Submit a job (or run a short command with --exec) on a worker, with a form and a review of the exact command",
		Long: `Submit a job with longarm run (or, with --exec, run a short command
synchronously with longarm exec) after reviewing the exact command.

Give a shell command line with -c (run with sh -c, or powershell -Command on a
Windows worker), or an argv after --. Without a command, or with
--interactive, a form asks for the worker, the mode and the command line.
Without a worker the hub places the job (auto).`,
		Example: `  lazylongarm job run                                     # the form
  lazylongarm job run worker-1 -c 'make test && echo ok'
  lazylongarm job run --exec worker-1 -- uname -a
  lazylongarm job run -c 'nvidia-smi' --require gpu=t4 --name gpu-check`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch dash := cmd.ArgsLenAtDash(); {
			case dash < 0 && len(args) > 1:
				return usage("give the command after --, or a shell line with -c")
			case dash < 0 && len(args) == 1, dash == 1:
				req.Worker, req.Argv = args[0], args[1:]
			case dash == 0:
				req.Argv = args
			case dash > 1:
				return usage("at most one worker before --")
			}
			if len(req.Argv) > 0 && req.Command != "" {
				return usage("give either -c or a command after --, not both")
			}
			form := interactive || (len(req.Argv) == 0 && req.Command == "")
			if form && !o.interactive(cmd) {
				return usage("no command: give -c '<line>' or a command after --")
			}
			_, client, err := o.hubClient(hubID)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			list, lerr := client.Workers(lctx)
			cancel()
			workers := liveWorkers(list.Nodes)
			if form {
				if lerr != nil {
					return fmt.Errorf("list workers: %w", lerr)
				}
				return o.jobRunForm(cmd, client, req, workers)
			}
			if req.Worker != "" && req.Worker != "auto" {
				if n, ok := workers[req.Worker]; ok {
					req.Windows = n.OS == "windows"
				}
			}
			return o.submitJob(cmd, client, req)
		},
	}
	f := c.Flags()
	f.StringVarP(&req.Command, "command", "c", "", "a shell command line (sh -c; powershell -Command on Windows workers)")
	f.BoolVar(&req.Exec, "exec", false, "run synchronously with longarm exec (no job; output here, exit code mirrored)")
	f.StringVar(&req.Name, "name", "", "job name")
	f.StringVar(&req.Cwd, "cwd", "", "working directory on the worker (default: home)")
	f.StringArrayVar(&req.Require, "require", nil, "KEY=VALUE labels the worker must have, e.g. os=linux (repeatable; jobs only)")
	f.DurationVar(&req.Timeout, "timeout", 0, "with --exec: kill the command after this long (longarm's default: 60s)")
	f.BoolVar(&interactive, "interactive", false, "open the form prefilled from the arguments")
	f.StringVar(&hubID, "hub", "", "hub id (default: the default hub)")
	return c
}

// liveWorkers are the online, non-revoked workers by name.
func liveWorkers(nodes []longarm.Node) map[string]longarm.Node {
	out := map[string]longarm.Node{}
	for _, n := range nodes {
		if n.IsWorker() && n.Online && n.State != "revoked" && n.State != "pending" {
			out[n.Name] = n
		}
	}
	return out
}

func (o *options) jobRunForm(cmd *cobra.Command, client *longarm.Client, req jobRequest, workers map[string]longarm.Node) error {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	names := make([]string, 0, len(workers))
	anyWindows := false
	for name, n := range workers {
		names = append(names, name)
		anyWindows = anyWindows || n.OS == "windows"
	}
	sort.Strings(names)
	choices := []wizard.Choice{{Value: "auto", Label: "auto — the hub places it on the worker with the most headroom"}}
	for _, name := range names {
		n := workers[name]
		choices = append(choices, wizard.Choice{Value: name, Label: fmt.Sprintf("%s  (%s/%s, %d jobs)", name, n.OS, n.Arch, n.JobsActive)})
	}
	worker := req.Worker
	if worker == "" {
		worker = "auto"
	}
	if _, ok := workers[worker]; !ok && worker != "auto" {
		return fmt.Errorf("worker %s is not online (online: %s)", worker, strings.Join(names, ", "))
	}
	command := req.Command
	if command == "" && len(req.Argv) > 0 {
		command = shellJoin(req.Argv)
	}
	timeout := ""
	if req.Timeout > 0 {
		timeout = req.Timeout.String()
	}
	mode := map[bool]string{true: "exec", false: "job"}[req.Exec]
	fields := []wizard.Field{
		{Key: "worker", Label: "Worker", Kind: wizard.Select, Value: worker, Options: choices, Required: true},
		{Key: "mode", Label: "Mode", Kind: wizard.Select, Value: mode, Options: []wizard.Choice{
			{Value: "job", Label: "job — longarm run: in the background, logs kept, shown in the Jobs tab"},
			{Value: "exec", Label: "exec — longarm exec: a short command now, output here (no job)"},
		}},
		{Key: "command", Label: "Command", Kind: wizard.Multiline, Value: command, Required: true, Help: "run with sh -c (powershell -Command on Windows): pipes, &&, $VARS work; several lines make a script"},
		{Key: "name", Label: "Job name", Value: req.Name, Help: "optional; jobs only"},
		{Key: "cwd", Label: "Directory", Value: req.Cwd, Help: "on the worker; default: its home"},
		{Key: "require", Label: "Requires", Value: strings.Join(req.Require, ","), Help: "auto jobs only: KEY=VALUE[,KEY=VALUE…] the worker must have, e.g. os=linux"},
		{Key: "timeout", Label: "Exec timeout", Value: timeout, Help: "exec only, e.g. 5m (default 60s)"},
	}
	for {
		values, err := wizard.EditDraft(ctx, wizard.Spec{Title: "Run on a worker", Description: fmt.Sprintf("%d online workers. The next screen shows the exact command before anything runs.", len(names)), SubmitLabel: "Review", Fields: fields}, in, out)
		if err != nil {
			return err
		}
		for i := range fields {
			fields[i].Value = values[fields[i].Key]
		}
		r := jobRequest{Worker: values["worker"], Exec: values["mode"] == "exec", Command: strings.TrimRight(values["command"], "\n"), Name: strings.TrimSpace(values["name"]), Cwd: strings.TrimSpace(values["cwd"])}
		if r.Worker == "auto" {
			r.Worker = ""
		}
		r.Windows = workers[r.Worker].OS == "windows"
		for _, q := range strings.Split(values["require"], ",") {
			if q = strings.TrimSpace(q); q != "" {
				r.Require = append(r.Require, q)
			}
		}
		if t := strings.TrimSpace(values["timeout"]); t != "" {
			d, perr := time.ParseDuration(t)
			if perr != nil {
				if _, cerr := wizard.Confirm(ctx, "Invalid timeout", fmt.Sprintf("%q is not a duration (e.g. 90s, 5m).", t), in, out); cerr != nil {
					return cerr
				}
				continue
			}
			r.Timeout = d
		}
		ok, err := wizard.ConfirmBack(ctx, "Review", r.reviewText(anyWindows), in, out)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		return o.submitJob(cmd, client, r)
	}
}

// submitJob hands the terminal to longarm run / exec.
func (o *options) submitJob(cmd *cobra.Command, client *longarm.Client, r jobRequest) error {
	if strings.TrimSpace(r.Command) == "" && len(r.Argv) == 0 {
		return usage("empty command")
	}
	c, err := client.Command(cmd.Context(), r.longarmArgs()...)
	if err != nil {
		return err
	}
	c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if !r.Exec {
		fmt.Fprintln(cmd.ErrOrStderr(), "Submitting; longarm prints the job id:")
	}
	if err := o.deps.Exec(c); err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			return fmt.Errorf("longarm %s exited with %d", c.Args[1], exit.ExitCode())
		}
		return err
	}
	if !r.Exec {
		fmt.Fprintln(cmd.ErrOrStderr(), "Follow it in the dashboard's Jobs tab or with: longarm logs <id> -f")
	}
	return nil
}
