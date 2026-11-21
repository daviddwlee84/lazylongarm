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
| Preview a peer (worker + its own longarm as a client, so sessions there can dispatch) | `lazylongarm peer add <ssh-alias> [--client-name N] [--client-user U] --dry-run --json` (admin certificate needed; show the user the warnings) |
| Workers (hub registry + managed machines) | `lazylongarm worker ls --json` |
| Users, and the fleet workers every user may use | `lazylongarm user ls --json` |
| Preview making this machine an admin of your Longarm user | `lazylongarm user self <user> [--device D] --dry-run --json` (the first user separates users) |
| Preview moving a fleet worker to a user | `lazylongarm worker add <ssh-alias> --reenroll --owner <user> --dry-run --json` |
| Invite someone (writes a file with a one-time code; never paste it into chat) | `lazylongarm user invite <user> --out <file> --yes --json` (refuses while fleet workers exist) |
| hc / herdr registration and dotfiles per machine | `lazylongarm companion status --json` (`--no-remote` skips SSH) |
| Preview registering machines in hc/herdr here | `lazylongarm companion remotes <machine>… \| --all [--tool hc\|herdr] --dry-run --json` |
| Preview a dotfiles bootstrap / update | `lazylongarm companion dotfiles <machine>… \| --all [--repo R] [--bundle B] [--answers defaults\|<preset>] [--update] --dry-run --json` (default `ask` needs the user's terminal) |
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
  `longarm-hub.service`; the panel listens on 127.0.0.1:7480
  (`lazylongarm hub panel` tunnels it for a human). `lazylongarm hub
  panel-access tailnet|public|loopback` can add a tailnet URL (tailscale serve)
  or public HTTPS; never switch to public without the user's explicit consent.
  `hub panel` opens a tailnet URL only when this machine is on that tailnet,
  else it falls back to the tunnel (`--tunnel` forces it).
- Worker add installs verified release binaries in `~/.local/bin` (`--fetch
  auto`, the default: the machine downloads them from GitHub, checked against
  the SHA256SUMS lazylongarm fetched, else they are pushed over ssh; `push`
  for machines without GitHub access; `remote` without fallback), mints a
  15-minute join code (admin join code + approve for admins, else an owner
  code), passes it over ssh stdin, installs the service and waits until
  `longarm worker ls --json` shows it online. The machine's own `longarm`
  stays signed out.
- A worker that stays offline: run `ssh <host> ~/.local/bin/longarmd doctor`
  first. Approved at the hub but no `~/.longarm/tls/node.crt` on the machine
  means longarmd there cannot reach the hub (proxy/TUN, firewall, or a clock
  that is far off; the probe warns above two minutes). Do not re-join it.
- Users: the first user separates users, fleet workers stay usable by
  everyone, and admin certificates get nothing on other users' owned
  workers. Order: `user self` → `worker add --reenroll --owner` for each
  fleet worker → `user remove <old certificate>` (no `--purge`) →
  `user invite`. Ask the user before each step; `user remove --purge` deletes
  job records.
- Peer add does the same, then enrolls the machine's `longarm` as client
  `<worker>-client` (client join code over ssh stdin) and runs `longarm ls`
  there. Revoke with `longarm admin revoke <worker>-client`.
- Companion setup never replaces another tool's state: machines already in hc
  or herdr, and dotfiles already set up, are skipped; dotfiles from a different
  repository are refused. The dotfiles bootstrap and `herdr machine add` may
  prompt, so let the user run the apply in their terminal.
- For jobs, use Longarm directly (`longarm run|jobs|logs|kill --json`); its
  `--json` output and exit codes are the stable contract (docs/programs.md).
