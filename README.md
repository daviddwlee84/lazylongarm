# lazylongarm

A keyboard-first console and setup wizard for [Longarm](https://github.com/lubobill1990/longarm)
fleets: install a `longarm-hub` on any SSH-reachable Debian/Ubuntu machine,
enroll this machine, turn SSH hosts (or this machine) into workers, and watch
workers and jobs in a lazygit-style dashboard.

Longarm itself has no SSH bootstrap and no TUI; lazylongarm adds both on top
of Longarm's stable CLI contract (`--json` output, exit codes, feature names).
It never reads Longarm's internal files or daemon API, never stores join codes
or passwords, and hands ssh/sudo prompts to your real terminal.

Docs: <https://daviddwlee84.github.io/lazylongarm/>, including
[why you would use it instead of plain `longarm`](https://daviddwlee84.github.io/lazylongarm/why/).

> Status: early (v0.1). Verified against Longarm v0.1.42 with a macOS client,
> an Ubuntu 24.04 hub on Azure and Linux + macOS workers.

## Install

macOS and Linux, amd64 and arm64 (lazylongarm drives the local `sh` and
OpenSSH, so Windows is not supported).

```sh
# Go 1.25+ (installs into $GOBIN, or $(go env GOPATH)/bin)
go install github.com/daviddwlee84/lazylongarm/cmd/lazylongarm@latest

# or a prebuilt archive from https://github.com/daviddwlee84/lazylongarm/releases
#   lazylongarm_<version>_<darwin|linux>_<amd64|arm64>.tar.gz  (+ checksums.txt)
tar -xzf lazylongarm_*_$(uname -s | tr A-Z a-z)_*.tar.gz lazylongarm && install -m 0755 lazylongarm ~/.local/bin/

lazylongarm upgrade --check     # later: lazylongarm upgrade
```

Shell completion: `lazylongarm completion zsh|bash|fish` (archives ship them
under `completions/`). Homebrew is planned.

**Requirements**: an OpenSSH client and key-based access to the machines you
manage; for `hub setup`, a Debian/Ubuntu host with systemd and `sudo`; the
Azure CLI (`az`, logged in) only for the optional NSG and `hub vm` features.
lazylongarm installs Longarm itself (official apt repository on the hub,
verified release binaries elsewhere).

## Build from source

```sh
go build -o bin/lazylongarm ./cmd/lazylongarm
```

## Quick start

```sh
bin/lazylongarm hub setup      # wizard: pick an SSH host, review the plan, apply
bin/lazylongarm worker add     # wizard: pick an SSH host or this machine
bin/lazylongarm peer add       # the same, and the machine's longarm can dispatch too
bin/lazylongarm                # dashboard: Fleet · Jobs · Hub
bin/lazylongarm hub panel      # Longarm's web panel through an SSH tunnel
bin/lazylongarm hub panel-access tailnet   # optional: panel on your tailnet (or public HTTPS)
```

Every mutating command builds a **plan** first. The Review screen shows the
exact upstream commands, files and ports (`#` = runs as root); nothing changes
until you choose Apply. The same plan is printed by `--dry-run` (`--json` for
machines), and scripts pass `--yes`. Reruns re-check every step and skip what
is already in place; the journal lives in `$XDG_STATE_HOME/lazylongarm/ops/`.

### What `hub setup` does (Debian/Ubuntu host)

1. Adds the official apt repository (signing key fingerprint verified) and
   installs `longarm-hub` (user `longarm-hub`, `longarm-hub.service`).
2. Writes `/etc/longarm-hub/hub.toml` (gRPC + relay on 7443/tcp+udp, STUN on
   7444/udp, panel on 127.0.0.1:7480, `[tailnet] provider = "none"`); a
   rerun that changes it restarts the hub.
3. Runs `longarm-hub bootstrap` **as the service user** (CA + one-time admin
   code), enables the service and waits for `/healthz`.
4. Checks listeners (hub ports up, previously listening sockets untouched).
5. Optionally opens the ports in the VM's Azure NSG (`az`, rules named
   `lazylongarm-*`), then dials the hub from this machine.
6. Installs `longarm` + `longarmd` into `~/.local/bin` (verified against the
   release's `SHA256SUMS`) and joins this machine as the hub's admin client.

### What `worker add` does

1. Installs verified `longarmd`/`longarm` for the machine's OS/arch into its
   `~/.local/bin`; a binary whose sha256 already matches the release's
   `SHA256SUMS` is left alone. `--fetch` picks how they get there:
   - `auto` (default): the machine downloads them from GitHub itself (`curl`,
     else `wget`) and checks each against the sha256 this machine read from
     `SHA256SUMS`; only then is it renamed into place. On any download failure
     (no route, 10 s connect timeout, under 32 KiB/s for 20 s, checksum
     mismatch) this machine pushes them instead and says so.
   - `push`: this machine downloads and verifies them once (cached) and copies
     them over ssh stdin, for machines without GitHub access. The copy is
     checked (size + sha256) before the rename and fails after 60 s without
     progress, not after a fixed time.
   - `remote`: the machine downloads them, no fallback; a mismatch installs
     nothing.
2. Mints a 15-minute join code (`longarm admin join-code --role worker` +
   `admin approve` with an admin certificate, else an owner code from
   `longarm worker add`) and passes it to `longarmd join` **over ssh stdin**
   (never in the review, logs or config).
3. Installs the service (`longarmd service install --user <you>`; sudo is
   handed to your terminal when it needs a password) or starts a user process.
4. Waits until `longarm worker ls --json` reports it online, then runs
   `longarm exec <worker> -- uname -a`.

The machine's own `longarm` stays signed out, so a shell on it cannot dispatch.

### What `peer add` does

Everything `worker add` does (steps already in place are skipped, so it also
upgrades an existing worker), then:

1. Mints a 15-minute **client** code (`longarm admin join-code --role client
   --name <worker>-client`; needs an admin certificate, no approval) and runs
   `longarm join` on the machine with the code from ssh stdin.
2. Runs `longarm ls` there: a herdr or ssh session on the machine can now run
   `longarm exec|run` against the other workers.

The client is its own node (node names are unique on the hub). Jobs on the
machine run as the account that holds it, so any job there can dispatch to
every worker you can use; the review warns, louder for root-equivalent
accounts (`docker`/`lxd`/`libvirt`/`disk` group, passwordless sudo). Take it
back with `longarm admin revoke <worker>-client`.

## Commands

| Command | Purpose |
|---|---|
| `lazylongarm` | dashboard (TTY); help otherwise |
| `hub setup` | install/repair a hub (wizard, or `--host` + `--address`) |
| `hub status` | service, healthz, reachability, NSG rules, enrollment |
| `hub panel [--create-user NAME]` | tunnel to the loopback panel, or open its tailnet/public URL (offers to create the first login) |
| `hub panel-user [NAME]` | create a web panel login (password + TOTP) |
| `hub panel-access [loopback\|tailnet\|public]` | reach the panel over SSH only, on your tailnet (installs Tailscale, `tailscale serve`), or on public HTTPS (Let's Encrypt) |
| `hub backup` | stop briefly, archive `/var/lib/longarm-hub` into the state dir (0600) |
| `hub vm status\|start\|stop\|restart\|delete` | the hub's Azure VM: power, size, traffic, cost estimate; reviewed lifecycle actions |
| `worker add [host] \| --local` | bootstrap a worker (wizard, or flags; `--fetch auto\|push\|remote`) |
| `peer add [host] \| --local` | a worker whose own longarm is also enrolled as a client (`--client-name`, `--client-user`) |
| `worker ls` | hub registry merged with machines lazylongarm manages |
| `companion status` | per machine: hc / herdr registration here, chezmoi dotfiles there |
| `companion remotes [machine…] \| --all` | register fleet machines in hc and herdr on this computer |
| `companion dotfiles [machine…] \| --all` | bootstrap (or `--update`) chezmoi dotfiles on fleet machines |
| `status [--doctor]` | configured hubs, local client, `longarm doctor` |
| `skill sync` | Longarm's skill into `~/.agents/skills` + Claude symlinks; lazylongarm stub skill |
| `settings show \| path` | effective configuration and paths |
| `--skill`, `skill print` | bundled agent operating guide |
| `upgrade [--check]` | upgrade lazylongarm the way it was installed (release archive, go install, Homebrew) |

Global: `--config`, `--json` (never prompts; errors as a JSON envelope on
stderr), `--color auto|always|never`. Exit codes: 0 ok, 1 failure, 2 usage,
130 canceled.

## Files

| Path | Content |
|---|---|
| `$XDG_CONFIG_HOME/lazylongarm/config.toml` | hubs, managed targets and `[companions]` settings (no secrets) |
| `$XDG_STATE_HOME/lazylongarm/ops/*.json` | per-operation journals (step statuses only) |
| `$XDG_CACHE_HOME/lazylongarm/releases/<tag>/` | verified Longarm release binaries |

XDG is used on macOS too. Longarm's own state stays where Longarm puts it
(`~/.config/longarm` for the client, `~/.longarm` for workers).

## Docs

- [docs/](docs/index.md) — operating guide, design, learning notes on the crypto/auth model
- [TODO.md](TODO.md) — future work; [pitfalls/](pitfalls/README.md) — known traps
- [CHANGELOG.md](CHANGELOG.md)

## License

MIT — see [LICENSE](LICENSE). lazylongarm is an independent tool built on
Longarm's public CLI contract and is not affiliated with the Longarm project.

## Development

```sh
go test ./... && go vet ./...
go build -o bin/lazylongarm ./cmd/lazylongarm && uv run scripts/pty_smoke.py
goreleaser release --snapshot --clean      # local release dry run
```

Releases: push a `vX.Y.Z` tag on `main`; CI reruns the tests and GoReleaser
creates a **draft** release (darwin/linux × amd64/arm64 + `checksums.txt`)
that is published after checking its assets.

Docs site (Zensical, English, `docs/` only). `.github/workflows/docs-check.yml`
builds and checks it on pull requests and publishes it to GitHub Pages from
`main`:

```sh
uv sync --locked --group docs
uv run --no-sync zensical build --strict && uv run --no-sync python scripts/check-docs-site.py
uv run --no-sync zensical serve --dev-addr 127.0.0.1:8000
```

The PTY smoke drives the real binary with fake `ssh`/`longarm` (isolated HOME,
XDG and PATH) and asserts that canceled wizards apply nothing.
