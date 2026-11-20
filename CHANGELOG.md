# Changelog

## Unreleased

- `peer add [host] | --local`: `worker add` plus a client enrollment of the
  machine's own `longarm` (`<worker>-client`, client join code over ssh stdin),
  so herdr/ssh sessions on it can dispatch to the other workers. Upgrades an
  existing worker in place; the review warns that jobs there can then dispatch
  too, louder for root-equivalent accounts. Dashboard Fleet tab: `P`.
- The host probe reports the account's Unix groups.
- Docs: `worker add` uses an admin join code when you hold an admin
  certificate (the docs said owner code).

## v0.2.0 — 2026-11-21

- `companion status|remotes|dotfiles`: register fleet machines in hc and herdr
  on this computer, bootstrap or update chezmoi dotfiles on them (built-in
  chezmoi install + `init --apply`, or your own bootstrap command template with
  `{bundle}` and friends), and show both per machine. Personal values live in
  `[companions.dotfiles]`; nothing already set up is replaced.
- `companion dotfiles --answers ask|defaults|<preset>`: chezmoi's first-init
  questions are asked in your terminal by default; presets in
  `[companions.dotfiles.presets]` pre-answer them (a preset named like the
  bundle is picked automatically); `--promptDefaults` only on request.
- Worker labels (from the hub) in the dashboard's Details and `worker ls`.
- Docs site: <https://daviddwlee84.github.io/lazylongarm/>, with a "Why
  lazylongarm" page.
- Dashboard Fleet tab: `R` registers the selected machine in hc/herdr, `D` sets
  up its dotfiles.
- The host probe also reports `git` and `chezmoi`; rerunning `worker add` keeps
  a target's `added_at` and companion state.

## v0.1.0 — 2026-11-21

First public release.

- `hub setup`: install longarm-hub on an SSH-reachable Debian/Ubuntu host from
  the official apt repository (signing key verified), loopback-only panel,
  bootstrap as the service user, optional Azure NSG rules, and enrollment of
  this machine as the hub's admin client (verified release binaries in
  `~/.local/bin`).
- `worker add`: bootstrap a worker over SSH or locally: verified binaries,
  one-time join code over ssh stdin (admin join-code + approve, or owned
  workers), systemd/LaunchDaemon service with sudo handed to the terminal, or a
  user process; waits until online and runs a first command; `--reenroll`.
- `hub status|panel|panel-user|backup`, `hub vm status|start|stop|restart|delete`
  (Azure), `worker ls`, `status [--doctor]`, `settings`, `skill sync`,
  `upgrade [--check]`, embedded agent skill (`--skill`).
- Dashboard: Fleet / Jobs (live log tail, follow, guarded kill) / Hub (checks,
  `longarm doctor`, background panel tunnel, panel logins).
- Shared plan engine: Review of exact commands, `--dry-run [--json]`, digest
  check, Check → Apply → Verify steps, journal and resume; OpenSSH-only
  transport with SSH-login and sudo terminal handoffs; secrets redacted
  everywhere.
