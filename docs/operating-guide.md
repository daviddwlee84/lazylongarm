# Operating guide

## Entry points

| Invocation | Behavior |
|---|---|
| `lazylongarm` in a terminal | dashboard |
| `lazylongarm` without a terminal | help |
| `hub setup` / `worker add` / `peer add` bare in a terminal | wizard |
| `hub setup --host H --address A` / `worker add HOST` / `peer add HOST` / `user self USER` | same plan from flags; Review in a terminal, `--yes` otherwise |
| `job run [worker] -c LINE` / `job run [worker] -- ARGV…` | submit a job (`--exec`: a short synchronous command); bare or `--interactive`: a form, then a review of the exact `longarm` command |
| `user invite USER` / `user remove USER` | one confirmation in a terminal, `--yes` otherwise (`invite` also accepts `--out FILE`) |
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
(0700 longarm-hub: `hub.db`, `ca/`, `bootstrap.env`). The panel listens on
loopback: `lazylongarm hub panel` (add `--create-user NAME` once for a password +
TOTP login). `lazylongarm hub panel-access` can also put it on your tailnet or
on public HTTPS ([Web panel](#web-panel)). Changing hub.toml on a rerun
restarts the hub (`restart-hub` step).

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
`--owner USER` (admins) mints an admin code owned by that user instead: it
joins approved, and only that user can use it. See [Users and sharing](#users-and-sharing).

`--reenroll` enrolls a machine that already has a certificate: it revokes the
hub's node of that name (after the new code exists, so a failed mint changes
nothing), joins with `--force`, keeps the old node's labels unless you pass
`--label` and its owner unless you pass `--owner` (an owned worker never
silently becomes a fleet worker), and restarts the service (it ends the
running longarmd as you, no sudo: the systemd unit's `Restart=always` with
`KillMode=process` and the LaunchDaemon's `KeepAlive` start it again and jobs
keep running; only when that fails does it use `sudo longarmd service stop`/`start`), because longarmd reloads certificates of
the same node only. The worker gets a new node ID; its port links go, its job
history stays with the old ID.

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

### When the worker stays offline

`worker add` waits up to two minutes for the hub to report the worker online.
If it does not, start with `ssh <host> ~/.local/bin/longarmd doctor`: it checks
the enrollment, certificate, daemon and hub link and says what to run next.
A Linux service logs to `journalctl -u longarmd`; a `--service user` daemon to
`~/.longarm/longarmd.out`; the macOS LaunchDaemon keeps no log file.

A worker that joined but has only `~/.longarm/tls/pending.key` (no
`node.crt`) has not collected its certificate yet. longarmd keeps asking the
hub and collects it once the hub approves it, so a rerun does not join again.
If the hub already shows the worker as approved, longarmd cannot reach the
hub: a proxy or firewall in the way (a Clash/mihomo TUN that lost its
upstream, for example), or a clock that is far off. The host probe warns
when the machine's clock differs from this one by more than two minutes;
proxies such as VMess and TLS both fail with a clock that is hours off.

## Users and sharing

A hub can serve several people, each with their own machines. The first
Longarm user separates users at once, and three upstream rules decide the
order of the steps:

- **Fleet workers** (no owner) are usable by every user, including a new one.
- A certificate without a user record (the admin certificate from `hub setup`)
  acts as a user named after itself, and **admins are not exempt** on other
  users' owned workers.
- A user-bound join code works for the user's **first device only**; later
  devices run `longarm login` and are approved at the panel's `/link` page
  (see [Later devices](#later-devices)). `peer add` does that for you once
  the user has a device.

| Step | Command | What happens |
|---|---|---|
| 1 | `lazylongarm user self alice --device alice-laptop` | backs up `<config>/tls`, `admin user add alice --admin --panel`, `longarm join --force` with the new code (the client daemon is restarted), checks `whoami`. Prints the panel password and TOTP once |
| 2 | `lazylongarm worker add HOST --reenroll --owner alice` per fleet worker | see [Worker add](#worker-add); refuses when this machine acts as another user (the worker would vanish from its view) or the user does not exist |
| 3 | `lazylongarm user remove <old-certificate>` | revokes the old admin certificate's implicit user (never `--purge` here: that also deletes its job records) |
| 4 | `lazylongarm user invite bob --out bob.md` | refuses while fleet workers exist (`--allow-fleet-workers` overrides); adds `bob` with a one-time client code and writes a 0600 Markdown invite |

The invite holds everything bob needs without anyone's SSH access or the web
panel: the release URLs and sha256 of `longarm`/`longarmd` per platform,
`longarm join <hub> <code> --ca-fingerprint <fp>`, then per machine
`longarm worker add <name>` (an owner code, no approval) and
`longarmd join … --name <name>` plus the service install. It also explains
what the hub's operator can see and suggests `[access] owners/users` and
`hub_job_details = false` in each machine's `longarmd.toml`.

Programs that loaded the old certificate before step 1 (`longarm mcp` in agent
sessions, the tray) keep using it; restart them. `status --doctor` warns about
fleet workers and an admin certificate without a user record.

### Later devices

Once a user has a device, a new machine of theirs signs in itself and is
approved in a browser (upstream's pairing, like an OAuth device code):

1. On the machine: `longarm login <hub> --ca-fingerprint <sha256> --no-browser`.
   It prints a `/link` URL and an 8-character code, and waits up to 10
   minutes. Always name the hub: without it, a machine whose `longarm` never
   signed in picks the hub built into the release (upstream's), not the one
   its worker uses, and prints a code that looks just as valid.
   `peer add` runs this step with the hub and fingerprint filled in.
2. Open the panel from a machine that reaches it (`lazylongarm hub panel`)
   and go to `/link` there: the URL `longarm login` prints is the gRPC host
   with the panel's port, which does not answer when the panel listens on
   loopback only (unless the hub sets `public_url`).
3. Sign in, type the code, check the host name and node key, approve. A
   certificate this machine already had for another user is removed.

Who may approve, on a hub without an identity provider:

| Panel login | How it is made | The device gets |
|---|---|---|
| the user's own (role user) | `admin user add U --panel`, or `user self U` / `user invite U --panel`, **only while `U` has no machine**: afterwards the hub refuses an admin's panel login for `U` | a client certificate |
| an admin login named `U` | `lazylongarm hub panel-user U` (`longarm-hub user add` on the hub host over SSH; the panel's own form refuses a user's name) | an **admin** certificate of `U` |

So keep `--panel` on `user self` unless your later devices may be admins.
An admin panel login that is not named like a user (the one `hub setup`
suggests) acts as a user of its own name: once users are separated it sees
none of your machines on the Nodes page.

Sharing stays off: an admin allows it per user (`longarm admin user set
alice --can-share`) and the owner shares one worker (`longarm worker share
<worker> --user bob`). Share Linux workers only with `[isolation] mode =
"per-user"` (jobs run as `la-<user>`); macOS has no per-user isolation.

## Peer add

`peer add` is `worker add` plus a client enrollment of the machine's own
`longarm`, so sessions on it (herdr, ssh, a handed-off agent) can dispatch to
the other workers. On a machine that is already a worker only the client steps
run.

| Choice | Meaning |
|---|---|
| Client name | the client node's name, default `<worker>-client` (node names are unique on the hub) |
| `--client-user` | the Longarm user the client acts as once the hub separates users; default: this machine's own user (the review says so). Also a wizard field |

- Needs an admin certificate (client codes come from `longarm admin
  join-code --role client`); they are issued without approval.
- When the client user already has a device, a join code cannot be issued
  (upstream issues user-bound codes for a user's first device only). The
  `link-client` step runs `longarm login <hub> --ca-fingerprint <fp>
  --no-browser --link` on the machine with the terminal handed over: approve
  its code at `/link` (`lazylongarm hub panel` in another terminal) as the
  login the review names (see [Later devices](#later-devices)). The hub names
  the device (`<user>-<hostname>`), so `--client-name` is ignored, and an
  admin panel login makes it an admin. Without a terminal (`--json`) the step
  stops with the exact command to run.
- The client steps check `longarm whoami` there: the user must be the client
  user, and the node name is recorded (shown as `client` in the dashboard's
  Details pane).
- An existing client certificate for the same hub is kept; one for another hub
  stops the plan (`--reenroll` replaces it).
- The client certificate lives in `~/.config/longarm` of the account jobs run
  as: any job on that machine can dispatch to every worker you can use. The
  review warns, and louder when the account is root-equivalent. Revoke with
  `longarm admin revoke <client node name>`.

## A second operator machine

To operate the hub from more than one machine (a laptop at home, a desktop
at work), make each one's longarm a client first (`lazylongarm peer add
<host>` from a machine that already operates the hub), then share the hub:

```sh
lazylongarm hub share work-desktop --targets --dry-run   # review
lazylongarm hub share work-desktop --targets             # apply
```

The plan checks that the machine's longarm is joined to the same hub (its
`hub.toml` address and the fingerprint of its `tls/ca.crt`) and that it has a
lazylongarm with `hub import`. Then it:

1. pins the hub host's SSH key on the machine, copied from this machine's
   `known_hosts` (never `ssh-keyscan`); the review shows the fingerprints;
2. adds the machine's existing default public key to the hub host's
   `authorized_keys` unless it can already ssh there (`--no-authorize` skips;
   lazylongarm never creates keys: without one, `ssh-keygen -t ed25519` there
   first, or name an existing key with `--identity ~/.ssh/<key>`: then a
   `Host lazylongarm-hub-<id>` alias with that `IdentityFile` goes into a file
   that an `Include` glob of its `~/.ssh/config` already picks up, e.g.
   `~/.ssh/config.d/*`; `~/.ssh/config` itself is never edited, and without
   such a glob the plan stops and names the line to add);
3. registers the hub in its lazylongarm config over ssh stdin (`hub import
   --stdin`); a `ssh_host` alias the machine resolves to another host is
   written as this machine's `user@hostname`;
4. runs `hub status` there.

With `--targets` the machines lazylongarm manages go along; aliases the other
machine resolves differently (or not at all) and this machine's own entry are
skipped with a warning. No private key or certificate travels. Each machine
keeps its own copy, so share again after `hub panel-access` or a hub move. Hub
VM and firewall checks there need that machine's own `az login`.

On a machine without a configured operator machine at hand, `lazylongarm hub
import --ssh-host <hub host>` derives the hub from its own joined longarm
(`I` on the dashboard's empty Hub tab).

## Colab workers

`worker colab add [name]` borrows a Google Colab VM as a temporary worker
through the Colab CLI (`uv tool install google-colab-cli`, signed in once):

1. `colab new -s <name>` (CPU by default; `--gpu T4|L4|A100|...`, `--high-mem`).
2. Installs `longarmd-linux-<arch>` in `/content/longarm/bin`. With
   `--fetch auto` (default) the VM downloads it from GitHub (about a second
   on Google's network) and keeps it only if it matches the release's
   SHA256SUMS fetched here; if that fails, this machine downloads and
   verifies it and `colab upload`s it (about 20 s; sha256 checked again on
   the VM). `--fetch remote` has no fallback, `--fetch push` always uploads.
3. Mints a 15-minute owner code (`longarm worker add`) and uploads it as a
   file that the VM reads and deletes before `longarmd join`: an exec'd cell
   and its `--env` values stay in the session's execution log, a file does not.
4. Starts `longarmd run` in the background (no service: it ends with the VM)
   and waits until the hub reports it online; it reaches the hub through the
   relay (outbound only).

The worker is yours only and is labeled `provider=colab` (plus
`gpu=<model>`), so `longarm run --require provider=colab` or `--on <name>`
targets it. The session has the worker's name. `worker colab ls` joins your
Colab sessions with `provider=colab` workers and shows the compute-unit
balance; `worker colab rm <name>` revokes the worker and stops the VM (either
half may already be gone) and refuses workers without the label.

- **Plan terms.** Colab allows remote shells and distributed-computing
  workers only on runtimes with a positive compute-unit balance (a paid plan;
  Google AI plans include one), so `add` refuses without one. Using a runtime
  as a proxy is forbidden on every plan: forward only to your own debugging
  ports.
- **Lifetime.** The VM bills until you remove it. Colab reclaims VMs (at
  most about 12 hours; idle runtimes sooner, and `colab status` shows a VM
  running only longarmd as IDLE): jobs running then are lost with their
  output, and the hub keeps an offline worker until `rm`. Copy results off
  first.
- **Placement.** Longarm has no way to keep auto-placed jobs off a worker:
  while a Colab worker exists, `longarm run` without a worker may land on it.
- **Revoked names** stay in `longarm ls` as `revoked` (the hub keeps the
  history); the name can be used again.

## Dashboard

| Tab | Shows | Actions |
|---|---|---|
| 1 Fleet | hub registry merged with managed targets: state, CPU, memory; details add path (relay/direct), labels, last seen. Revoked workers are hidden (`t` shows them) | `n` run a job on it (`job run`) · `a` add worker · `C` add a Colab worker · `P` add peer · `x`/Enter run `uname -a` · `s` `longarm ssh` · `R` register in hc/herdr · `D` dotfiles · `A` approve a pending node (admins) · `X` revoke (confirm; a Colab worker: `worker colab rm`, which also stops its VM) |
| 2 Jobs | active jobs (and the last 30 min; `t` shows all) with a live log tail that follows new output until you scroll up; a multi-line command (a `bash -lc` script) is one row, shown in full above the log | `n` run a job (`job run`, worker `auto`) · `f` follow log (`longarm logs -f`) · `K` kill (confirm, `--if-created-at`) |
| 3 Hub | hub profile, `hub status` checks (incl. panel logins), `longarm doctor` | `S` hub setup · `p` panel (tunnel on/off, or the tailnet/public URL) · `U` panel login · `A` panel access |

The hub keeps revoked workers' records for good (they feed its revocation
list), so there is nothing to delete; Fleet and `worker ls` hide them, like
`longarm admin nodes` without `--all`.

After `n`, the form returns to the Jobs tab with the new job selected (`exec`
mode runs in the handed-over terminal and creates no job).

Global: `1 2 3`/←→/h l tabs · ↑↓/j k select · `gg`/`G` · Tab focus the right
pane (j/k, PgUp/PgDn scroll it) · `y` copy the job log, the details or the open
panel URL · `r` refresh (auto every 5 s; `[tui] refresh_seconds`) · `M` mouse
on/off · `?` help · `q` quit. A failed refresh keeps the last rows and marks
them stale in the header.

The mouse is captured by default: click a tab, a row or a footer hint (hints
run on release, and only if nothing changed since the press), scroll the pane
under the pointer with the wheel, and drag over the right pane to copy whole,
untruncated lines. Copies go to the clipboard through the terminal (OSC 52,
which also works over SSH where the terminal allows it) and through the local
clipboard tool (`pbcopy`, `xclip`, …). While the mouse is captured, most
terminals still select text natively with Shift- or Option-drag; `M` releases
the mouse for the session and `[tui] mouse = false` makes that the default.

Colors follow `--color auto|always|never` (`NO_COLOR` is honored in auto);
without color, focus and state stay visible through markers (`▌`, `›`, `●/○`,
`[x]`, bracketed buttons).

## Companions

lazylongarm knows the fleet's machines, so it can set up the tools you use next
to Longarm without retyping hosts. Machines are targets (by name) or SSH hosts;
bare in a terminal each command offers a picker; `--all` takes every managed
machine. Both setup commands are plans (`--dry-run [--json]`, Review, `--yes`)
and leave work that is already done alone.

| Command | What it does |
|---|---|
| `companion status [--no-remote]` | hc / herdr columns from their machine lists here; dotfiles and ferry columns from one read-only check over SSH (chezmoi source, origin, revision; ferry version and which backends are signed in) |
| `companion remotes` | for each machine hc or herdr does not list: `hc machine add <ssh-host> --install --name <machine>` (copies hc there when missing or old) and `herdr machine add <ssh-host> --label <machine>` (installs and starts the herdr server there; approve its prompts in the terminal). `--tool hc\|herdr` limits it; a tool not installed here is skipped with a warning |
| `companion remotes --transport longarm` | registers `longarm-<worker>` instead of the SSH host (see below); arguments may be Longarm worker names. A machine hc or herdr already reaches over its SSH host is left alone unless `--switch` replaces that entry (its terminals keep running; hc allows one entry per machine) |
| `companion ssh-config` | writes the `longarm-<worker>` OpenSSH aliases (see below); `--remove` undoes it |
| `companion dotfiles` | bootstraps chezmoi dotfiles in your terminal (`ssh -tt`, so sudo prompts work), then records the bundle and revision on the target. A machine that already has dotfiles is skipped (`--update` runs `chezmoi update --init`); one with dotfiles from another repository is refused, never replaced |
| `companion ferry setup` | installs ferry and a backend CLI, then runs the backend's own login in your terminal (see [Bulk transfers with ferry](#bulk-transfers-with-ferry)) |
| `companion ferry pair` | writes the same ferry job on a sender and a receiver and checks both sides |

### Reaching workers through Longarm (`longarm-<worker>`)

Every worker runs a built-in sshd that is reachable only through
`longarm proxy <worker>`, over the hub's mTLS channel: no ssh keys, no
direct route and no open port (it picks a direct or relayed path like any
Longarm connection). `longarm ssh-config` prints OpenSSH entries for it;
`companion ssh-config` puts them where ssh, scp, VS Code Remote-SSH, hc and
herdr find them:

1. the output of `longarm ssh-config <worker…>` (which also records the
   workers' host keys, fetched over mTLS, in Longarm's `known_hosts`) is merged
   into `~/.ssh/lazylongarm/longarm.conf`, a file lazylongarm owns;
2. a marked block goes at the very top of `~/.ssh/config` (an Include below a
   `Host` line would only apply to that host), with a backup in
   `~/.ssh/config.lazylongarm.bak`:

   ```
   # >>> lazylongarm (longarm ssh aliases) >>>
   Include ~/.ssh/lazylongarm/*.conf
   # <<< lazylongarm <<<
   ```

3. `ssh -G longarm-<worker>` must show `ProxyCommand … proxy <worker>`.

An Include that already covers the file (yours, or a glob such as
`Include ~/.ssh/conf.d/*.conf` with `[companions.ssh] file` pointing there)
counts, and nothing is edited. When chezmoi manages `~/.ssh/config`, or with
`--include no`, lazylongarm does not edit it (chezmoi apply would undo that)
and prints the line instead: put it at the top of the file's source once;
OpenSSH skips a glob that matches nothing, so the line is safe on every
machine. `--include yes` edits the file anyway.

```toml
[companions.ssh]
file = "~/.ssh/lazylongarm/longarm.conf"   # default
include = "auto"                           # auto | yes | no
```

Shells and commands run as the account longarmd runs as (the SSH user for
workers `worker add` installed). A worker without a direct path is relayed
through the hub, which is slower than a direct tailnet ssh.

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

### Bulk transfers with ferry

`longarm cp`, `put` and `get` move data directly between two machines when
they can reach each other. Otherwise the data goes through the hub's relay,
which is rate-limited (`relay_rate`), counts as the hub's network egress (often
billed), and uses the hub VM's CPU. Longarm itself refuses large relayed copies
and suggests "cloud storage both sides use".
[ferry](https://github.com/daviddwlee84/ferry) does that: it packs units,
pushes them to a relay both machines reach (Baidu Pan through BaiduPCS-Go, any
rclone remote, or a GitHub draft release) and verifies every part with
sha256 on the way in. Transfers resume after a restart, and ferry can cap
bandwidth, set a daily quota and restrict transfers to certain hours.

```sh
lazylongarm companion ferry setup worker-1 worker-2              # baidupcs by default
lazylongarm companion ferry setup worker-1 worker-2 --backend rclone
lazylongarm companion ferry pair worker-1:~/datasets/run42 worker-2:/srv/inbox
lazylongarm companion ferry pair worker-1:/data/exports worker-2:~/in \
  --via rclone --remote gdrive: --glob '*.parquet' --extract
```

`setup` installs uv into `~/.local/bin` when missing and then ferry with
`uv tool install`, from `[companions.ferry] install`, else a ferry version this
lazylongarm pins (sender and receiver share relay formats that may change
between 0.x versions, so machines set up together get the same ferry). A
ferry older than 0.2.0 is upgraded; `--upgrade` reinstalls from the source,
which aligns machines set up at different times. The `baidupcs` backend downloads BaiduPCS-Go from its GitHub
releases into `~/.local/bin`; rclone and gh come from the system's package
manager. A backend that is not signed in gets its own login (`BaiduPCS-Go
login`, `rclone config`, `gh auth login`) in your terminal over `ssh -tt`.
lazylongarm checks only *whether* a backend is signed in, never which account,
and records the ferry version and signed-in backend names on the target.
`--no-login` skips the logins and says what to run.

`pair <sender>:<path> <receiver>:<path>` checks that both machines run the
same ferry (at least 0.2.0) and have the relay's backend signed in, then writes **one** job file,
`~/.config/ferry/jobs/<name>.toml`, on both (ferry's own wizard uses the same
location). It refuses to replace a different file there. Then it runs
`ferry doctor` on each side and `ferry plan` on the sender, and records the
pair under `[[companions.ferry.jobs]]`. Paths are on each machine: absolute,
`~/…`, or relative to its home directory.

| Choice | Default | Flag |
|---|---|---|
| units | each entry of a directory source; a file source is one unit | `--glob` (relative to the directory) |
| relay | `baidupcs`, prefix `/ferry/<name>` | `--via rclone --remote R`, `--via ghrelease --repo owner/name [--tag T]`, `--via rsync [--ssh-host H] [--landing DIR]`, `--prefix` |
| destination | tar archives kept under the destination | `--extract` unpacks (each unit path is replaced whole) |
| sources | kept (`delete_source = "never"`) | `--delete-source after_remote_size\|after_receiver_ack` |
| egress | `direct` (`ferry doctor` checks the route to the relay) | edit the job file |
| start | nothing runs; the next steps are printed | `--service` installs ferry's systemd user units (Linux), receiver first |

`--via rsync` skips the cloud: the sender pushes over ssh into a landing
directory on the receiver (default `~/.cache/ferry/<name>/landing`), and the
receiver reads it locally. You still get ferry's units, sha256 checks,
resumption and change detection. The sender needs non-interactive ssh (a key)
to the receiver. `--ssh-host` is how the *sender* reaches it (default: the
receiver's SSH host here, which must also resolve there), and `ferry doctor`
on the sender checks the connection. Use it only on a direct route (LAN,
tailnet). A host that goes through `longarm proxy` (a `longarm-<worker>`
alias) takes the hub's relay whenever the two workers have no direct path,
and so does `longarm cp`, so the plan warns when it sees one. It also warns
when it sees a `ProxyJump`.

```toml
[companions.ferry]
# install = "git+https://github.com/daviddwlee84/ferry@<tag or commit>"   # override the pinned version
backends = ["baidupcs"]
```

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

The hub's panel listens on 127.0.0.1:7480 on the hub host and is served at
`/`; without a login you only see the hub's front page.

- Dashboard, Hub tab: `p` opens a **background** tunnel (the dashboard stays
  usable; `p` again or quitting closes it) and your browser; `U` manages
  logins (`hub panel-user`: list, create, reset, remove). Creating or
  resetting asks for the password twice, then prints a **QR code** to scan
  with an authenticator app (plus the secret and `otpauth://` URI, once).
  Log in with user + password + the app's current 6-digit code. If the secret
  leaks, `hub panel-user NAME --reset`. The Hub checks show how many logins exist.
- CLI: `lazylongarm hub panel` keeps the tunnel in the foreground until Ctrl+C
  and offers to create the first login.

### Panel access: loopback, tailnet or public

`lazylongarm hub panel-access [loopback|tailnet|public]` (or `A` on the Hub tab)
adds one more way to reach the panel. The SSH tunnel keeps working in every
mode, and each run is a reviewed, rerunnable plan; switching modes undoes the
previous one.

| Mode | Who can reach the login page | Protected by | What lazylongarm changes on the hub |
|---|---|---|---|
| `loopback` (default) | whoever can SSH to the hub | SSH key + password + TOTP | nothing (undoes the other modes) |
| `tailnet` | devices on your tailnet | tailnet membership + password + TOTP | installs Tailscale from its apt repository (key `2596A99E…957F5868` verified), `tailscale up --accept-dns=false` (terminal handed over: open the printed URL, or `--auth-key-file`), `tailscale serve --bg --https=443 http://127.0.0.1:7480` |
| `public` | the whole Internet | password (argon2id, per-IP failure limit) + TOTP | systemd drop-in `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `public_url` / `https = ":443"` / `https_redirect = ":80"` / `acme_email` in hub.toml, restart, NSG rules `lazylongarm-panel-https` (443/tcp) and `lazylongarm-panel-http` (80/tcp) |

- **tailnet** needs MagicDNS and HTTPS Certificates switched on in the tailnet's
  DNS settings. The URL is `https://<hub>.<tailnet>.ts.net/`; open it on any
  device logged in to the tailnet (install Tailscale on your phone). Funnel is
  never used. Going back to loopback stops `tailscale serve` but leaves
  Tailscale installed and logged in (`sudo tailscale logout` if unwanted).
- **public** needs a public hub whose DNS name (`--domain`, default: the hub
  address) points at it. longarm-hub gets the certificate from Let's Encrypt
  itself (TLS-ALPN-01 on 443; port 80 only serves the redirect, `--no-redirect`
  keeps it closed); the check waits up to 2 minutes for it. `public_url` also
  becomes the base URL of the hub's install scripts. Rerunning `hub setup` keeps
  these settings. Sign-in with Google and others is not configured yet.
- `hub status` adds a `panel-access` check (HTTPS answer and certificate days
  left, or whether `tailscale serve` still proxies the panel), and `hub panel`
  / `p` open the URL directly (`hub panel --tunnel` forces the tunnel). A
  tailnet URL is opened only when this machine's Tailscale is connected to
  that tailnet (`tailscale status --json`, also the CLI inside
  `/Applications/Tailscale.app` on macOS); when it is not installed, stopped,
  logged out or on another tailnet, both say why and use the SSH tunnel
  instead.

Ctrl+C during any terminal handoff from the dashboard (wizards, `hub panel`,
`longarm logs -f`) ends only that command and returns to the dashboard.

## Security model in one paragraph

The hub's CA key can mint certificates that run commands on every worker (as
the user longarmd runs as). Treat the hub host as the fleet's root of trust:
key-only SSH, as few other public services as possible, and only machines you
are willing to expose to that host's compromise. lazylongarm keeps the panel
on loopback unless you choose `hub panel-access tailnet` (your tailnet only) or
`public` (the login page on the Internet: password + TOTP are then the only
barrier to an admin-level UI), uses one-time 15-minute codes passed over stdin,
verifies downloads against `SHA256SUMS`, and records only non-secret values.
