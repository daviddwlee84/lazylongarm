# Changelog

## Unreleased

- `companion ferry setup` installs a pinned ferry (the 0.2.0 release commit;
  ferry has no tags yet), so machines set up together match, and upgrades a
  ferry older than 0.2.0 (0.1.0 receivers trust paths from the relay).
- `companion ferry pair` refuses machines whose ferry versions differ or are
  older than 0.2.0, and names the `setup … --upgrade` that aligns them.
  `companion status` says when versions differ or are outdated.
- `companion ferry pair --via rsync [--ssh-host H] [--landing DIR]`: push over
  ssh from the sender into a landing directory on the receiver. The plan warns
  when the sender reaches that host through `longarm proxy` (the hub's relay
  without a direct path) or a ProxyJump.
- `companion ferry setup` no longer lists a BaiduPCS-Go install step on
  machines that already have it.

## v0.7.0 — 2026-11-27

- `companion ferry setup [machine…] | --all`: install
  [ferry](https://github.com/daviddwlee84/ferry) (uv into `~/.local/bin`
  when missing, then `uv tool install`) and a backend CLI (`--backend
  baidupcs|rclone|gh`; BaiduPCS-Go comes from its GitHub releases), then run
  the backend's own login in your terminal. Only whether a backend is signed
  in is checked and recorded, never the account. `--no-login`, `--upgrade`.
- `companion ferry pair SENDER:PATH RECEIVER:PATH`: write the same ferry job
  (`~/.config/ferry/jobs/<name>.toml`) on both machines, refusing to replace
  a different file, then run `ferry doctor` on each side and `ferry plan` on
  the sender. Bulk data then goes through a cloud relay (Baidu Pan, an rclone
  remote, a GitHub draft release) instead of the hub's relay. Sources are
  kept and nothing starts unless `--service` (systemd user units on Linux).
- `companion status` has a FERRY column (version, signed-in backends) and
  reads dotfiles and ferry state in one SSH round trip per machine.
- Config: `[companions.ferry]` (`install`, `backends`, `[[jobs]]`); targets
  record `ferry_version`, `ferry_backends`, `ferry_checked_at`.

## v0.6.0 — 2026-11-26

- The dashboard can change the fleet, not only show it. Fleet: `n` runs a
  job on the selected worker, `C` adds a Colab worker, `X` removes a Colab
  worker (stopping its VM) or revokes any other worker after a confirm, `A`
  approves a pending node (admins). Jobs: `n` runs a job (worker `auto`).
  After a job is sent the dashboard switches to Jobs and selects it.
- `job run [worker] [-c LINE | -- argv] [--exec]`: a form (worker, job or
  exec mode, command, name, cwd, requirements) that hands the command line
  to `sh -c` (PowerShell on Windows workers) and shows the exact `longarm
  run` / `longarm exec` command, quoting included, before sending it.
- `worker colab add --interactive`: a form for name, accelerator and high
  RAM that shows the compute-unit balance.
- Revoked workers are hidden by default: `t` on the Fleet tab and
  `worker ls --all` show them (`--json` still lists every worker). The hub
  keeps revoked records for its revocation list, so they cannot be deleted.
- Fix: a job whose command spans several lines (`bash -lc "…"`) no longer
  breaks the Jobs layout; the list shows it on one line and the detail pane
  shows the full script above the log. Job text from the hub is stripped of
  terminal escape sequences.

## v0.5.2 — 2026-11-26

- Finding the latest release (lazylongarm's `upgrade`, Longarm's for
  `worker add` / `hub setup`) no longer fails when GitHub's unauthenticated
  API limit (60 requests an hour per IP, quickly used up behind a shared
  proxy) is exhausted: it uses `GH_TOKEN` / `GITHUB_TOKEN` when set and
  falls back to the `github.com/<repo>/releases/latest` redirect.
- `upgrade` on a Homebrew install treats an unknown latest release as a note
  and lets Homebrew decide, instead of failing with "HTTP 403" (a v0.5.1
  regression).
- `upgrade --check --json` leaves out `up_to_date` when the latest release is
  unknown (it used to report `false`), adds `supported` (same as
  `apply_supported`) and, for Homebrew installs, `formula`
  (`daviddwlee84/tap/lazylongarm`); `upgrade` runs brew with that full name,
  so a same-named formula elsewhere is never the one upgraded.

## v0.5.1 — 2026-11-25

- `upgrade` on a Homebrew install runs `brew update` before `brew upgrade`
  (brew skips its own auto-update for a while and then only warns "already
  installed") and checks the version Homebrew ended up with: a tap that has
  not picked up the release yet is an error instead of "lazylongarm
  upgraded". `upgrade --check` now reports the latest release for Homebrew
  installs too, and a Homebrew install that is up to date says so.

## v0.5.0 — 2026-11-25

- `hub share <host> [--targets] [--no-authorize]`: register this hub on
  another operator machine whose longarm is joined to it (checked: hub
  address and CA fingerprint). Pins the hub host's SSH key there from this
  machine's `known_hosts`, adds its existing public key to the hub host's
  `authorized_keys` when it cannot ssh there yet, imports the hub entry over
  ssh stdin, and checks `hub status` from there. `--targets` brings the
  managed machines whose aliases resolve the same there. `--identity <key>`
  uses an existing key there and writes a `Host lazylongarm-hub-<id>` alias
  into a file its `~/.ssh/config` already Includes (never the file itself).
- `hub import`: register a hub derived from this machine's joined longarm
  (`--ssh-host` to manage the hub host too), or from `hub share` with
  `--stdin`. A hub with the same address and CA is updated in place; another
  hub under the same id is refused (`--id`).
- The dashboard's empty Hub tab offers `I` (import), and "no hub" errors name
  `hub import` / `hub share`.

## v0.4.3 — 2026-11-25

- `peer add` handles a later device of a user: when the client user already
  has a device (upstream issues user-bound codes for a user's first device
  only), the new `link-client` step runs `longarm login <hub>
  --ca-fingerprint <fp> --no-browser --link` on the machine with the terminal
  handed over, to approve at the panel's `/link`. The review says who can
  approve and that an admin panel login makes the device an admin. Without a
  terminal it stops with the exact command, so a bare `longarm login` never
  signs in to the hub built into the release. Previously the plan was refused,
  even in `--dry-run`, and even when the machine was already signed in.
- `peer add --client-user` defaults to this machine's own user once the hub
  separates users; the wizard has a Client user field (it used to stop with
  "pass --client-user").
- The client steps check that `longarm whoami` on the machine acts as the
  client user, and record its node name (the hub's device name after `/link`).
- The dashboard's Details pane shows a peer's client node.
- The later-device hint no longer suggests `longarm admin issue --role client
  --user` on the hub host (the hub host has no `longarm`, and `longarm-hub
  issue` has no `--user`).

## v0.4.2 — 2026-11-23

- Shell completion completes SSH hosts from `~/.ssh/config` (`worker add`,
  `peer add`, `hub setup --host`), hub ids (`--hub`), fleet machines and
  worker names (`companion remotes|ssh-config|dotfiles`) and fixed flag
  values; commands and flags that take no path no longer offer file names.
- Homebrew: `brew install daviddwlee84/tap/lazylongarm` (installs bash/zsh/fish
  completion); README documents completion setup for other installs.
- `worker colab add|ls|rm`: borrow a Google Colab VM as a temporary worker
  owned by you, through the Colab CLI. `add` creates the session, installs
  longarmd (the VM downloads it from GitHub and it must match SHA256SUMS
  fetched here; `--fetch push` uploads a copy verified here), enrolls it with a
  15-minute owner code passed as a file (never in an exec'd cell), starts
  `longarmd run` in the background and waits until it is online; it refuses
  without a positive compute-unit balance (Colab's terms for remote shells
  and workers). Workers are labeled `provider=colab` (+ `gpu=`); `rm` revokes
  the worker and stops the VM, and refuses workers without that label.

## v0.4.1 — 2026-11-22

- `user self --panel=false` warns in the review that the user's panel login
  cannot be added later (the hub refuses an admin's panel login for a user
  with machines): later devices are then approved at `/link` by an admin
  login named like the user, and become admins.
- `peer add` and `user self`, refusing a user who already has a device, say
  who can approve `longarm login` at `/link` (the user's panel login, or an
  admin login named like the user via `hub panel-user`), to open `/link` via
  `hub panel` (the URL login prints fails for a loopback panel), and mention
  `longarm admin issue --role client --user`.
- Operating guide: "Later devices" (pairing steps, who may approve, what the
  device gets).

## v0.4.0 — 2026-11-22

- `user self|invite|ls|remove`: share your hub with other people. `user self`
  creates your Longarm user and switches this machine to an admin
  certificate of it (old certificate backed up, panel login for `/link`
  printed once); `user invite` adds someone with a one-time code and writes a
  0600 Markdown invite (binaries and sha256, join, owned workers, privacy),
  and refuses while fleet workers exist; `user remove` revokes a user's
  devices and workers.
- `worker add --owner USER`: an admin join code owned by that user (joins
  approved, no approve step). It refuses when this machine acts as another
  user or the user does not exist.
- `worker add --reenroll` revokes the hub's node of that name after minting
  the new code, keeps its labels and its owner (an owned worker never becomes
  a fleet worker by accident), and restarts the service so longarmd loads
  the new certificate (it previously kept the old identity until a restart).
- `worker add` on a machine lazylongarm already manages keeps its saved
  service mode (`system`/`user`) unless `--service` is given.
- `worker ls` and the dashboard show one row per re-enrolled machine (its
  live node; the hub also lists the revoked old one).
- `worker ls` has an OWNER column (`fleet` for workers without one);
  `status` shows the user on hubs that separate users, and `status --doctor`
  warns about fleet workers and an admin certificate without a user record.
- `peer add --client-user U` refuses once `U` has a device (upstream issues
  user-bound codes for a user's first device only) and explains `longarm login`
  with `/link` instead.

- `companion ssh-config [worker…] | --all`: OpenSSH aliases `longarm-<worker>`
  that reach a worker's built-in sshd through `longarm proxy` (keyless, over
  the hub's mTLS channel, no direct route or open port). The output of
  `longarm ssh-config` is merged into `~/.ssh/lazylongarm/longarm.conf`, and a
  marked `Include ~/.ssh/lazylongarm/*.conf` block goes at the top of
  `~/.ssh/config` (backup `~/.ssh/config.lazylongarm.bak`; `--remove` takes it
  out byte for byte). When chezmoi manages `~/.ssh/config`, or with
  `--include no`, the file is left alone and the line to add is printed. An
  Include that already covers the file (also a glob) counts. Settings:
  `[companions.ssh] file, include`.
- `companion remotes --transport longarm`: register machines in hc and herdr
  as `longarm-<worker>` (aliases set up first), so workers you cannot ssh to
  directly open in hc/herdr too; arguments may be Longarm worker names.
  Machines registered over their SSH host are left alone unless `--switch`
  replaces those entries (hc allows one entry per machine). `companion
  status` marks such entries `(longarm)`, or `+longarm` next to an SSH entry.

## v0.3.2 — 2026-11-21

- `hub panel` and the dashboard's `p` open a tailnet panel URL only when this
  machine's Tailscale is connected to that tailnet (`tailscale status --json`;
  on macOS also the app's bundled CLI). Not installed, stopped, logged out or
  on another tailnet: they say why and open the SSH tunnel instead.

## v0.3.1 — 2026-11-21

- `worker add` no longer reports "already enrolled" for a worker that joined
  but never collected its certificate (only `tls/pending.key`). It says so,
  still skips joining again (that would leave a second pending node), and when
  the hub shows the worker approved but offline at the timeout, the error says
  the worker cannot reach the hub (proxy, firewall or clock).
- The "did not come online" fix is now `longarmd doctor` on the machine; the
  old hint pointed macOS at `~/.longarm/longarmd.log`, which the LaunchDaemon
  never writes. Linux and `--service user` logs are named in the error.
- The host probe reads the machine's clock: probes, `worker add`/`peer add`
  and `hub setup` reviews warn when it differs from this machine's by more
  than two minutes.

## v0.3.0 — 2026-11-21

Upgrading: v0.2.0 refuses a config written by `peer add` from this release
(unknown `client_name`); upgrade every lazylongarm that shares the config.
From now on, older builds warn about unknown keys instead of failing.

- `hub panel-access [loopback|tailnet|public]`: reach the web panel on your
  tailnet (installs Tailscale from its apt repository with the signing key
  pinned, hands the terminal to `tailscale up` for the login URL or takes
  `--auth-key-file`, then `tailscale serve --bg --https=443`; never funnel) or
  over public HTTPS (longarm-hub's built-in Let's Encrypt, a systemd drop-in
  for binding 443/80, NSG rules); `loopback` undoes both. Dashboard Hub tab:
  `A`; `p` and `hub panel` open the tailnet/public URL directly (`--tunnel`
  forces the tunnel); `hub status` checks it.
- `hub setup` restarts the hub when a rerun rewrites hub.toml (it used to keep
  running with the old file) and keeps the panel access settings.
- The host probe reports the distro codename.
- `peer add [host] | --local`: `worker add` plus a client enrollment of the
  machine's own `longarm` (`<worker>-client`, client join code over ssh stdin),
  so herdr/ssh sessions on it can dispatch to the other workers. Upgrades an
  existing worker in place; the review warns that jobs there can then dispatch
  too, louder for root-equivalent accounts. Dashboard Fleet tab: `P`.
- The host probe reports the account's Unix groups.
- `worker add|peer add --fetch auto|push|remote` (also in the wizard): the
  machine can download the Longarm binaries from GitHub itself (curl, else
  wget), checked against the sha256 this machine read from `SHA256SUMS` before
  an atomic rename; a mismatch installs nothing. `auto`, the new default, tries
  that with tight bounds (10 s connect, abort under 32 KiB/s for 20 s) and
  pushes from this machine on any failure, saying why; `push` is the previous
  behavior; `remote` has no fallback. The Review / `--dry-run [--json]` shows
  the exact curl command per binary.
- Pushing binaries no longer fails on slow links after a fixed 10 minutes: a
  copy fails after 60 s without progress (the receiving side reports its byte
  count), with an overall deadline scaled to the file size. Slow copies log
  their progress. The receiving side checks size and sha256 before the rename,
  so a copy cut short can never be renamed into place.
- A rerun after an interrupted binaries step installs only the binary that is
  missing or different; one already identical is no longer copied again. The
  check no longer downloads the binaries on this machine, only `SHA256SUMS`.
- Dashboard and wizards redesigned: a shared theme (16 ANSI colors, readable
  in light and dark terminals; `NO_COLOR`/`--color never` keep the markers),
  rounded panes with column headers, centered help/kill modals (kill defaults
  to Cancel), and a job log that follows its tail until you scroll up. Wizards
  are a one-column form with distinct focus, cursor and choice markers.
- Mouse on by default: click tabs, rows and footer hints, wheel-scroll the
  pane under the pointer, drag over the right pane to copy whole lines (OSC 52
  plus the local clipboard). `y` copies the job log, details or panel URL; `M`
  toggles the mouse, `[tui] mouse = false` turns it off by default.
- Fix: overlapping `worker add` runs (or any two commands that record to the
  config, such as `hub setup` and `companion dotfiles`) no longer lose the
  second run's record with "config file changed since it was loaded; reload
  and retry". Each save now re-reads the config under its lock and changes
  only its own target or hub, so a retry of the step also succeeds instead of
  failing again with the same stale copy. `peer add` records the same way.
- Fix: a config written by a newer lazylongarm (for example a field it added to
  a `[[targets]]` entry) no longer stops an older build with "strict mode:
  fields in the document are missing in the target struct". The older build
  loads it, prints one warning on stderr naming the unknown keys (such as
  `targets[worker-1].pool_name`) and suggesting `lazylongarm upgrade`, and
  keeps those keys when it writes the file; removing a hub or target removes
  its keys with it. `settings show` lists them under `unknown_keys`. Broken
  TOML, type mismatches (now with the line number) and invalid references
  still fail.
- `worker add --service user`: the "Start longarmd in the background" step no
  longer skips itself as "already in place" when no daemon is running. Its
  `pgrep -f` pattern matched the checking shell's own command line; it is now
  anchored to a command line that starts with `longarmd run`.
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
