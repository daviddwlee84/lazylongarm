# Operating guide

## Entry points

| Invocation | Behavior |
|---|---|
| `lazylongarm` in a terminal | dashboard |
| `lazylongarm` without a terminal | help |
| `hub setup` / `worker add` / `peer add` bare in a terminal | wizard |
| `hub setup --host H --address A` / `worker add HOST` / `peer add HOST` | same plan from flags; Review in a terminal, `--yes` otherwise |
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
| Binaries (`--fetch`) | `auto` (default): the machine downloads them from GitHub, else this machine pushes them · `push`: always copy from this machine · `remote`: the machine downloads them, no fallback |

With an admin certificate, lazylongarm adds **fleet** workers (`longarm admin
join-code --role worker`, approved right after they join). Without one it
mints an owner code (`longarm worker add`): the worker is yours, pre-approved
and usable only by you until you `longarm worker share` it. `--mint` picks one.

### How the binaries get there

The trust anchor is the same in every mode: this machine reads the release's
`SHA256SUMS` from GitHub (cached in `$XDG_CACHE_HOME/lazylongarm/releases/<tag>/`)
and every installed binary must hash to its entry. First the step compares the
sha256 of `~/.local/bin/longarmd` and `longarm` on the machine; matching ones
are left alone, so a rerun after an interrupted pair installs only the missing
one.

| `--fetch` | What runs | Bounds |
|---|---|---|
| `auto` (default) | the machine runs `curl` (else `wget`) against the release URL into a temporary file, checks the sha256 passed on its command line, then `chmod 0755` + rename. On any failure this machine pushes instead, and the output says why | connect 10 s; abort under 32 KiB/s for 20 s (GNU wget: 20 s read timeout; BusyBox wget: `-T 20`). One failed download switches the rest of the step to push |
| `push` | this machine downloads and verifies the assets (cached), then streams each over ssh stdin into a temporary file; the machine checks size + sha256 before the rename | fails after 60 s without new bytes arriving; overall deadline 2 min + size at 16 KiB/s (about 45 min for 40 MB) |
| `remote` | like `auto`, without the fallback: a failed or mismatching download stops the step and installs nothing | connect 30 s; abort under 1 KiB/s for 60 s |

`auto` costs a machine without GitHub access at most a few seconds (DNS or
connection refused) to about 30 s (dropped packets, then a stuck proxy) once
per run, before the usual push. A checksum mismatch on the machine's own
download is reported as such (something on its network path may alter
downloads) and never installed; `auto` then pushes this machine's verified
copy. Choose `push` for machines that must not reach GitHub at all, and
`remote` when this machine's uplink is the slow side. This machine (`--local`)
always installs from its own verified cache.

The Review, `--dry-run` and `--dry-run --json` list the exact `curl` command,
URL and destination for each binary.

## Peer add

`peer add` is `worker add` plus a client enrollment of the machine's own
`longarm`, so sessions on it (herdr, ssh, a handed-off agent) can dispatch to
the other workers. On a machine that is already a worker only the client steps
run.

| Choice | Meaning |
|---|---|
| Client name | the client node's name, default `<worker>-client` (node names are unique on the hub) |
| `--client-user` | the Longarm user the certificate belongs to; required once the hub separates users |

- Needs an admin certificate (client codes come from `longarm admin
  join-code --role client`); they are issued without approval.
- An existing client certificate for the same hub is kept; one for another hub
  stops the plan (`--reenroll` replaces it).
- The client certificate lives in `~/.config/longarm` of the account jobs run
  as: any job on that machine can dispatch to every worker you can use. The
  review warns, and louder when the account is root-equivalent. Revoke with
  `longarm admin revoke <worker>-client`.

## Dashboard

| Tab | Shows | Actions |
|---|---|---|
| 1 Fleet | hub registry merged with managed targets: state, path (relay/direct), CPU, memory | `a` add worker · `P` add peer · `x`/Enter run `uname -a` · `s` `longarm ssh` · `R` register in hc/herdr · `D` dotfiles |
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
answers = "ask"                                # ask | defaults | <preset> (see below)
default_bundle = "server"

[companions.dotfiles.bundle_by_os]
darwin = "laptop"
linux = "server"

# Pre-answers for chezmoi's prompt*Once questions. Keys are the question texts
# from your .chezmoi.toml.tmpl, e.g. promptChoiceOnce . "profile" "Machine profile" …
[companions.dotfiles.presets.server]
"Machine profile" = "ubuntu_server"
"Install Tailscale" = true

# Optional: your own bootstrap instead of the built-in chezmoi install + init
# bootstrap = "curl -fsSL https://raw.githubusercontent.com/you/dotfiles/main/bootstrap.sh | bash -s -- --yes --bundle {bundle}"
```

### Answering chezmoi's prompts

A dotfiles repository usually asks questions on first init (profile, which tools
to install, …). chezmoi stores the answers on the machine and never asks
`prompt*Once` questions again, so a wrong answer sticks. The built-in bootstrap
(`chezmoi init --apply <repo>`) answers them by `--answers` (or `answers` in the
config):

| Mode | What happens |
|---|---|
| `ask` (default) | chezmoi asks each question in your terminal, exactly as a manual `chezmoi init` would. Needs a terminal; without one the plan stops instead of guessing |
| `<preset>` | `[companions.dotfiles.presets.<preset>]` pre-answers questions (`--promptString`/`--promptChoice` for strings, `--promptBool`, `--promptInt`). chezmoi matches them by **question text**, not by variable name: for `promptChoiceOnce . "profile" "Machine profile" …` the key is `"Machine profile"`. Commas and `=` cannot appear in keys or values. The rest are asked in the terminal, or take their defaults when there is none. A preset named like the machine's bundle is picked automatically, so `bundle_by_os` selects presets per OS |
| `defaults` | every question takes the template's default (`--promptDefaults`). Only when you ask for it |

With `--update`, new questions are asked in a terminal (defaults without one).

### Your own bootstrap

`bootstrap` replaces the built-in install + init with your command; it then
answers the questions itself. Placeholders `{repo}` `{bundle}` `{name}` `{os}`
`{arch}` `{distro}` become shell-quoted values (`${VAR}` is left to the shell);
an unknown placeholder, or one used without a value, stops the plan. The bundle
is `--bundle`, else the target's `dotfiles_bundle`, else `bundle_by_os[<os>]`,
else `default_bundle`. Leave a non-interactive flag such as `--yes` out of the
template to get your script's own interactive setup in the terminal.

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
