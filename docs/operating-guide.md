# Operating guide

## Entry points

| Invocation | Behavior |
|---|---|
| `lazylongarm` in a terminal | dashboard |
| `lazylongarm` without a terminal | help |
| `hub setup` / `worker add` bare in a terminal | wizard |
| `hub setup --host H --address A` / `worker add HOST` | same plan from flags; Review in a terminal, `--yes` otherwise |
| `--interactive` with some flags | wizard prefilled from the flags |
| partial flags (`hub setup --host H`) | usage error (exit 2), never a wizard |
| `--dry-run [--json]` | print the plan, change nothing |
| `--json` | never prompts; errors are `{"error":{...}}` on stderr |

## Wizard keys

| Screen | Keys |
|---|---|
| Forms | Tab/Shift+Tab move · ↑↓/j k choose in a select · `/` search a select · Ctrl+S submit from any field · Esc back |
| Review | ↑↓ scroll · Tab switch Back/Apply (default **Back**) · Enter confirm · Esc back |
| Apply | first Ctrl+C: stop after the current step · second: abort it |
| Failed step | choose Retry (done steps are re-checked and skipped) or Quit (rerun the same command later) |

SSH logins (password, passphrase, host-key approval) and sudo passwords are
never collected by lazylongarm: the terminal is handed to `ssh`/`sudo` and
returned afterwards. Without a terminal these become `ssh-auth-required` /
`sudo-password-required` errors with a fix command.

## Hub setup

Requirements on the hub machine: Debian/Ubuntu with systemd, `sudo`, `curl`,
`gpg`, and free ports 7443/tcp, 7443/udp, 7444/udp, 127.0.0.1:7480.

| Choice | Meaning |
|---|---|
| Address nodes dial | DNS name/IP workers and clients connect to; written as `public_addr` and the certificate SAN |
| Network `public` | reachable from the Internet; open the ports in the cloud firewall |
| Network `private` | reachable only on your network (e.g. the host's tailnet IP); no public exposure |
| Cloud firewall `azure` | find the VM by FQDN/IP across `az` subscriptions, add `lazylongarm-hub-tcp/udp` to its NIC/subnet NSGs |
| Enroll this machine | install `longarm` into `~/.local/bin` and join as admin |
| Auto-approve | let non-owned workers join without `longarm admin approve` |

Paths on the hub: `/etc/longarm-hub/hub.toml` (root:longarm-hub 0640, the
previous file is backed up as `hub.toml.bak-*`), `/var/lib/longarm-hub`
(0700 longarm-hub: `hub.db`, `ca/`, `bootstrap.env`). The panel is loopback
only: `lazylongarm hub panel` (add `--create-user NAME` once for a password +
TOTP login).

### Recovering

- Rerun the same command: every step checks first and skips what is done.
- A step marked **unknown** (e.g. the connection dropped while enrolling)
  blocks the next run until you inspect it; then `--force-resume`.
- `lazylongarm hub status` lists service, healthz, reachability, NSG rules and
  enrollment with fix hints.
- Hub logs: `ssh <hub> sudo journalctl -u longarm-hub -n 100`.

## Worker add

| Choice | Meaning |
|---|---|
| Worker name | name in the hub; defaults to the SSH alias |
| Labels | `KEY=VALUE` for scheduling (`--require` on `longarm run`) |
| Run longarmd as `system` | `longarmd service install --user <you>` (systemd unit / macOS LaunchDaemon), survives reboots, sudo once |
| Run longarmd as `user` | background `longarmd run` as you; no sudo; restart after a reboot |
| Direct over tailnet | also listen on the tailnet IP (plus an explicit hub relay listener) for direct paths between tailnet peers; default is relay only (outbound connections) |

Workers you add with lazylongarm are **owned** by you (`longarm worker add`):
pre-approved and usable only by you until you `longarm worker share` them.

## Dashboard

| Tab | Shows | Actions |
|---|---|---|
| 1 Fleet | hub registry merged with managed targets: state, path (relay/direct), CPU, memory | `a` add worker · `x`/Enter run `uname -a` · `s` `longarm ssh` · `R` register in hc/herdr · `D` dotfiles |
| 2 Jobs | active jobs (and the last 30 min; `t` shows all) with a live log tail | `f` follow log (`longarm logs -f`) · `K` kill (confirm, `--if-created-at`) |
| 3 Hub | hub profile, `hub status` checks (incl. panel logins), `longarm doctor` | `S` hub setup · `p` panel tunnel on/off · `U` panel login |

Global: `1 2 3`/←→/h l tabs · ↑↓/j k select · `gg`/`G` · Tab focus details ·
`r` refresh (auto every 5 s; `[tui] refresh_seconds`) · `?` help · `q` quit.
A failed refresh keeps the last rows and marks them stale in the header.

## Companions

lazylongarm knows the fleet's machines, so it can set up the tools you use next
to Longarm without retyping hosts. Machines are targets (by name) or SSH hosts;
bare in a terminal each command offers a picker; `--all` takes every managed
machine. Both setup commands are plans (`--dry-run [--json]`, Review, `--yes`)
and leave work that is already done alone.

| Command | What it does |
|---|---|
| `companion status [--no-remote]` | hc / herdr columns from their machine lists here; a dotfiles column from a read-only check over SSH (chezmoi source, origin, revision) |
| `companion remotes` | for each machine hc or herdr does not list: `hc machine add <ssh-host> --install --name <machine>` (copies hc there when missing or old) and `herdr machine add <ssh-host> --label <machine>` (installs and starts the herdr server there; approve its prompts in the terminal). `--tool hc\|herdr` limits it; a tool not installed here is skipped with a warning |
| `companion dotfiles` | bootstraps chezmoi dotfiles in your terminal (`ssh -tt`, so sudo prompts work), then records the bundle and revision on the target. A machine that already has dotfiles is skipped (`--update` runs `chezmoi update --init`); one with dotfiles from another repository is refused, never replaced |

lazylongarm only calls hc's and herdr's public CLIs; it never edits their state
files. Your personal values go in the config:

```toml
[companions.dotfiles]
repo = "https://github.com/you/dotfiles.git"   # URL, "user" or "user/repo"
# Optional: your own bootstrap instead of the built-in
#   chezmoi install (get.chezmoi.io → ~/.local/bin) + chezmoi init --apply --promptDefaults <repo>
bootstrap = "curl -fsSL https://raw.githubusercontent.com/you/dotfiles/main/bootstrap.sh | bash -s -- --yes --bundle {bundle}"
default_bundle = "server"

[companions.dotfiles.bundle_by_os]
darwin = "laptop"
linux = "server"
```

Placeholders in `bootstrap`: `{repo}` `{bundle}` `{name}` `{os}` `{arch}`
`{distro}`, replaced by shell-quoted values (`${VAR}` is left to the shell). An
unknown placeholder, or one used without a value, stops the plan. The bundle is
`--bundle`, else the target's `dotfiles_bundle`, else `bundle_by_os[<os>]`,
else `default_bundle`. Without a bootstrap template, prompts are answered with
their defaults; put the bundle logic in your dotfiles' own bootstrap script
when you need per-machine answers.

## Hub VM (Azure)

When hub setup opened the Azure firewall, the hub profile records the VM
(`[hubs.cloud]`: subscription, resource group, VM, NSG, rules). `hub vm` drives
it through `az`:

| Command | What it does |
|---|---|
| `hub vm status` | power state, size, disk, address, egress this month (Azure Monitor) and a monthly cost estimate from the public retail price API (list prices; credits not applied) |
| `hub vm start` | `az vm start`, then waits until the hub answers on its address |
| `hub vm stop` | `az vm deallocate` (compute billing stops; IP and disk still billed). The hub goes offline: workers keep running their jobs but cannot get new ones |
| `hub vm restart` | `az vm restart`, then waits for the hub |
| `hub vm delete` | deletes the whole resource group — only one tagged `managed-by=lazylongarm,purpose=longarm-hub` — after a Review and typing the group's name; needs a backup from the last 7 days in the backups dir (or `--backup-confirmed`); then forgets the hub and its targets in the config |

All of them are plans (`--dry-run`, Review, `--yes`). `hub status` and the
dashboard's Hub tab include the VM's power state. With several Azure tenants
signed in, lazylongarm passes `--subscription` so `az` picks the right token.

## Agent skills

`lazylongarm skill sync` follows the "one canonical directory + symlinks"
layout:

| Path | What |
|---|---|
| `~/.agents/skills/longarm/` | `longarm skill install --agent codex`: a copy of the skill bundled in the installed binary, with a manifest. Longarm rewrites it when a new release first runs (tray/daemon start or the first `longarm` command), unless you modified it. Codex, Copilot CLI and Gemini CLI read this directory. |
| `~/.claude/skills/longarm` | relative symlink to the above (Longarm leaves symlinks alone, so it never fights the link) |
| `~/.agents/skills/lazylongarm/SKILL.md` | stub: same frontmatter, body says "run `lazylongarm --skill`" — the guide always matches the binary |
| `~/.claude/skills/lazylongarm` | relative symlink to the stub |

Prefer the Longarm-managed copy over `npx skills add lubobill1990/longarm`:
the latter pins a GitHub commit whose flags can be newer than your binary.
Don't also run a bare `longarm skill install` (it would put a real copy for
Claude next to the symlink).

## Web panel

The hub's panel listens on 127.0.0.1:7480 on the hub host only and is served
at `/`; without a login you only see the hub's front page.

- Dashboard, Hub tab: `p` opens a **background** tunnel (the dashboard stays
  usable; `p` again or quitting closes it) and your browser; `U` manages
  logins (`hub panel-user`: list, create, reset, remove). Creating or
  resetting asks for the password twice, then prints a **QR code** to scan
  with an authenticator app (plus the secret and `otpauth://` URI, once).
  Log in with user + password + the app's current 6-digit code. If the secret
  leaks, `hub panel-user NAME --reset`. The Hub checks show how many logins exist.
- CLI: `lazylongarm hub panel` keeps the tunnel in the foreground until Ctrl+C
  and offers to create the first login.

Ctrl+C during any terminal handoff from the dashboard (wizards, `hub panel`,
`longarm logs -f`) ends only that command and returns to the dashboard.

## Security model in one paragraph

The hub's CA key can mint certificates that run commands on every worker (as
the user longarmd runs as). Treat the hub host as the fleet's root of trust:
key-only SSH, as few other public services as possible, and only machines you
are willing to expose to that host's compromise. lazylongarm keeps the panel
on loopback, uses one-time 15-minute codes passed over stdin, verifies
downloads against `SHA256SUMS`, and records only non-secret values.
