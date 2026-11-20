---
name: lazylongarm
description: Set up and operate a Longarm fleet (longarm-hub, workers over SSH, jobs) with the lazylongarm CLI. Use when asked to install a Longarm hub, add a machine as a Longarm worker, check hub/worker health, or inspect Longarm jobs through lazylongarm.
---

# lazylongarm

lazylongarm wraps the Longarm CLI (`longarm`, `longarmd`, `longarm-hub`,
https://github.com/lubobill1990/longarm) with reviewed setup plans and a
dashboard. It never stores join codes or passwords; prompts (ssh login, sudo)
are handed to the real terminal, so **agents must use the non-interactive
forms below** and pass `--yes` only after showing the user the `--dry-run`
plan.

## Commands for agents

| Goal | Command |
|---|---|
| What is configured, is this machine enrolled | `lazylongarm status --json` (add `--doctor`) |
| Hub health (service, healthz, ports, NSG, enrollment) | `lazylongarm hub status --json` |
| Preview a hub install | `lazylongarm hub setup --host <ssh-alias> --address <dns-or-ip> [--cloud azure\|none] --dry-run --json` |
| Apply it (after the user approved the plan) | same flags plus `--yes --json` |
| Preview adding a worker | `lazylongarm worker add <ssh-alias> [--name N] [--label k=v] [--service system\|user] --dry-run --json` |
| Apply it | same plus `--yes --json` |
| Workers (hub registry + managed machines) | `lazylongarm worker ls --json` |
| Effective config and paths | `lazylongarm settings show` |

Errors are a JSON envelope on stderr: `{"error":{"code","message","fix"?,"step"?}}`.
Codes: `usage` (exit 2), `ssh-auth-required` (ask the user to run
`ssh <host>` once), `sudo-password-required` (ask the user to run the command
in their terminal, or use `--service user`), `step-failed`, `step-unknown`
(inspect the host; rerun with `--force-resume` only after the user agrees),
`canceled` (exit 130).

## Behaviour to rely on

- Every apply is a plan of steps with Check → Apply → Verify. Rerunning the
  same command skips steps already in place; the journal is in
  `$XDG_STATE_HOME/lazylongarm/ops/`.
- Hub setup targets Debian/Ubuntu via the official apt repository and
  `longarm-hub.service`; the panel listens on 127.0.0.1:7480 only
  (`lazylongarm hub panel` tunnels it for a human).
- Worker add pushes verified release binaries to `~/.local/bin`, mints a
  15-minute owner code with `longarm worker add`, passes it over ssh stdin,
  installs the service and waits until `longarm worker ls --json` shows it
  online.
- For jobs, use Longarm directly (`longarm run|jobs|logs|kill --json`); its
  `--json` output and exit codes are the stable contract (docs/programs.md).
