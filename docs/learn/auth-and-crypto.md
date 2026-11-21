# Learning notes: how this fleet's crypto and identity work

> Written 2026-11-20 against Longarm v0.1.42. Examples use an imaginary fleet:
> hub `hub.example.com`, worker `worker-1`, admin client `laptop`.
> "Per the docs" marks content from Longarm's documentation or source code;
> "inferred" marks conclusions drawn from observed behavior.

## The one-sentence version

**The hub is a private certificate authority (CA).** Each machine generates its
own private key locally when it joins, then trades a one-time join code for a
certificate signed by the hub. From then on machines confirm each other's
identity and encrypt traffic with **mTLS** (both sides present a certificate).
**People** log in to the web panel through a different scheme: a password plus
TOTP (two-factor). And **lazylongarm manages the machines over SSH**, a third,
separate trust relationship.

```
              you (a person)
   ┌──────────┼──────────────────────────────┐
   │ SSH key  │ password + TOTP               │ your admin certificate (mTLS)
   ▼          ▼                              ▼
 machines   web panel (127.0.0.1 only)     longarm CLI ──mTLS──► hub ◄──mTLS── workers
 (setup /   reached through an SSH tunnel         └──── mTLS (direct, or via the hub relay) ────► worker
  repair)
```

## 1. Basic terms (these are enough to follow the rest)

| Term | Plain meaning | In this system |
|---|---|---|
| Public / private key | A key pair: keep the private key secret, publish the public key; anything signed with the private key can be verified by anyone with the public key | Every machine, the hub's CA and your SSH identity each have one |
| Certificate | A signed statement that "this public key belongs to so-and-so", including validity period and purpose (role) | The "pass" the hub signs for each machine |
| CA (certificate authority) | Whoever signs certificates; trusting it means trusting what it signs | The hub itself is the CA (`/var/lib/longarm-hub/ca/`) |
| Fingerprint | A SHA-256 digest of a key or certificate, short enough to compare by eye | The hub CA fingerprint, e.g. `3f9a…c2e1` |
| TLS | The protocol behind HTTPS: verify the other side's identity, then set up an encrypted channel | The basis of every Longarm connection |
| mTLS (mutual TLS) | Ordinary HTTPS only has the server present a certificate; mTLS has **both sides** present one and verify each other | Used between hub, clients and workers everywhere |
| TOFU | Trust On First Use: trust whatever answers the first time, then remember it | SSH host keys and `--ca-fingerprint` exist to turn TOFU into "compare first, then trust" |

## 2. How a machine joins the fleet (enrollment)

Using `worker-1` and the output of `lazylongarm worker add`:

1. **An admin creates a one-time join code**
   (`longarm admin join-code --role worker --name worker-1 --ttl 15m`).
   - Per the docs: the code is 6 characters, single-use and time-limited (15
     minutes here). The hub's database stores only its hash.
   - The code is just an admission ticket. It is not a certificate and cannot
     run anything.
2. **The worker generates its private key locally** (per the docs: "The key is
   generated here; the hub signs a certificate"). The private key **never
   leaves the worker**.
3. **The worker first checks that the hub is the real hub**: it compares the
   hub's CA certificate with `--ca-fingerprint 3f9a…` and refuses on a
   mismatch. The `hub CA verified (3f9a…)` line in the output is this step. It
   stops an impostor hub from harvesting the join code (a man-in-the-middle
   attack).
4. **The code is traded for a certificate**: the worker sends its public key and
   the code; the hub checks the code and signs a certificate with its CA key,
   carrying the name (worker-1), the role (worker) and an expiry.
5. **Approval**: with `auto_approve = false` a new worker starts as `pending`;
   lazylongarm, acting as admin, immediately runs
   `longarm admin approve worker-1`. This is a second gate: even a leaked code
   does not let an unknown machine in without someone approving it.
6. From then on the worker uses the certificate to hold an mTLS connection to
   the hub, report its state and accept jobs.

Certificate lifetimes (`[certs]` in `hub.toml`): 90 days for workers, 30 days
for clients and admins, renewed automatically before expiry. The
`renew_before: 864000s` in `whoami` means renewal starts 10 days before expiry.

**Extra protection from lazylongarm**: the join code reaches the worker over
**ssh stdin** (`read -r code`). It never appears on the Review screen, in logs or
in the config file, and it does not land in the remote shell history.

**Owned workers skip step 5.** On a hub with users, a code minted with
`--owner alice` (`lazylongarm worker add HOST --owner alice`, or alice's own
`longarm worker add`) makes a worker that joins already approved and that only
alice may use: nobody else can run anything on it, so there is nothing for an
admin to approve. The certificate itself is the same kind; the owner is a
record on the hub, pushed to the worker, which then refuses everyone else
(admins included). A worker cannot change owner in place; it enrolls again
with a new node ID (`worker add --reenroll --owner`).

## 3. What happens on a connection (mTLS)

```
client (laptop)                                  worker (worker-1)
  presents: laptop's certificate (admin, by hub CA)  presents: worker-1's certificate (worker, by hub CA)
  checks: signed by the hub CA? right name?          checks: signed by the hub CA? does the role allow it?
              └──────────── both pass → encrypted channel ────────────┘
```

- **The path can be direct** (for example `tcp://100.x.y.z:7701` on a tailnet,
  or `longnet/direct` after a successful QUIC hole punch) **or go through the
  hub relay** (when hole punching fails).
- **Through the relay, the hub cannot read the content**: the mTLS session
  between client and worker is end to end; the relay only forwards encrypted
  bytes. This is TLS inside TLS: the outer TLS goes to the relay, the inner mTLS
  runs end to end. It protects privacy, but this traffic shape is also easier to
  recognize with deep packet inspection, so keep the hub on its own host rather
  than sharing an IP with unrelated services.
- **The hub controls who may join**, not the content of each connection. If the
  hub goes down, jobs that are already running are unaffected (per the docs:
  the control plane and data plane are separate).

## 4. How people log in to the web panel (password + TOTP)

The panel is a web page for people. It uses an account, a password and
**TOTP** rather than machine certificates:

| Factor | Something you know or have | Stored where |
|---|---|---|
| Password | something you know | the hub's database keeps only a hash (inferred: the usual practice, no plaintext) |
| TOTP code | something you have (an authenticator app on your phone) | the seed (secret) is on the hub and on your phone |

**How TOTP works (RFC 6238)**:

1. When the account is created, the hub generates a random secret (a Base32
   string like `JBSWY3DPEHPK3PXP…`) and hands it to your app as a QR code (an
   `otpauth://` URI).
2. From then on the hub and the app **each** compute an HMAC of "secret +
   current time (in 30-second steps)" and take 6 digits. Matching numbers prove
   you hold the secret.
3. So the 6 digits change every 30 seconds while **the secret stays the same**
   (until you reset the account).
   - A leaked secret means someone else can compute your codes → reset it
     (`hub panel-user NAME --reset`).
   - A single leaked 6-digit code matters little; it expires within 30 seconds.
4. The phone's and the hub's clocks must be roughly right; a large skew makes
   every check fail.

**Why the panel binds to 127.0.0.1**: nobody on the network can reach it;
you first open an SSH tunnel (with your SSH key). So by default the panel is
effectively protected by three layers: SSH key, password and TOTP.

`lazylongarm hub panel-access` trades the first layer for convenience:

| Mode | First layer | What an attacker must get past |
|---|---|---|
| `loopback` | your SSH key | SSH key, password, TOTP |
| `tailnet` | membership of your tailnet (`tailscale serve`, HTTPS with a `*.ts.net` certificate) | your tailnet account, password, TOTP |
| `public` | none: the login page is on the Internet (HTTPS, Let's Encrypt) | password and TOTP; longarm-hub limits failed logins per IP, hashes passwords with argon2id and uses `HttpOnly`, `Secure`, `SameSite=Strict` session cookies |

In public mode a pre-authentication bug in the panel would also be reachable
by everyone, which is why the tailnet is the recommended way to open the
panel from a phone.

## 5. The trust lazylongarm itself relies on (SSH and software sources)

| Link | How impersonation is ruled out |
|---|---|
| SSH to the VM | The host key must be confirmed on first connect. Preferably fetch its fingerprint first with `az vm run-command` (through Azure's own channel, not SSH) and compare it with the `SHA256:…` ssh shows: compare first, then trust, rather than blind TOFU |
| Logging in to the VM | An SSH key (`~/.ssh/id_ed25519`); password login is disabled |
| sudo | `sudo -n` is tried in the background first; when a password is needed the terminal is handed to you and lazylongarm never sees it |
| apt packages | After downloading Longarm's apt signing key, its fingerprint `CA72074C…5307` is compared before it is trusted; apt then verifies the repository's signatures with it. `hub panel-access tailnet` does the same for Tailscale's key (`2596A99E…957F5868`) |
| Worker binaries | Downloaded from the GitHub release and checked against the same release's `SHA256SUMS` (minisign signature checks are not done yet; see TODO) |
| Reading the hub CA fingerprint | Read with `longarm-hub ca show` over the already-verified SSH connection, so the fingerprint is trustworthy |

The **chain of trust** is: Azure account → the VM's host key → SSH → the hub CA
fingerprint → every machine certificate. Skip verifying any link and the
guarantees after it weaken.

## 6. What needs the most protection (and what happens if it is stolen)

| Secret | Where it lives | If stolen | Mitigation |
|---|---|---|---|
| **Hub CA private key** | the hub VM's `/var/lib/longarm-hub/ca/ca.key` (readable only by the longarm-hub user) | **Worst case**: certificates for any role can be signed → commands run as longarmd on every worker | Run the hub on a dedicated VM that only runs the hub; keep an encrypted `hub backup` offline |
| Your admin certificate and key | `~/.config/longarm/tls/` on your laptop | Someone can manage the fleet as admin until the certificate is revoked or expires (30 days) | `longarm admin revoke`; use a separate non-admin client day to day |
| A worker's private key | `~/.longarm/tls/` on each worker | Someone can impersonate that worker (and receive other people's jobs) | Revoke the worker and enroll it again |
| Join code | exists only during the 15 minutes of joining | Usable to join within its lifetime, but `auto_approve = false` still requires admin approval | Short lifetime, single use, passed over stdin |
| TOTP secret | the hub's database and your phone | Together with the password, someone can log in to the panel | Never paste it into chats or logs; reset it if it leaks |
| Panel password | your memory or a password manager | Still needs TOTP and SSH (or the tailnet) to log in; with public panel access only TOTP remains | Use a password manager |
| SSH private key | `~/.ssh/` on your laptop | Someone can log in to the machines (lazylongarm's management channel) | Add a passphrase; use an agent |

**Who jobs run as on a worker**: longarmd's service account (a normal user, not
root). So the blast radius of a compromised fleet is everything that account
can reach: SSH keys, git credentials, data. That is why only non-sensitive
machines should join during a trial. macOS workers have no per-user isolation.

## 7. Resetting after a leak or loss (and whose permissions that takes)

There are **two independent management paths** that can rescue each other:

| Path | What it needs | What it can do |
|---|---|---|
| **A. Admin certificate** | `~/.config/longarm` on your laptop (`laptop (admin)`) | `longarm admin approve/revoke/join-code`, `longarm worker remove`: day-to-day management |
| **B. SSH plus sudo on the hub host** | `~/.ssh/id_ed25519` and sudo on the VM (the Azure account can fall back to `az vm run-command`) | `longarm-hub …`: create or reset panel accounts, **mint a new admin join code**, read the CA, back up. This is the break-glass path |

| Leaked or lost | What to do | Path |
|---|---|---|
| TOTP secret or panel password | `lazylongarm hub panel-user NAME --reset` (new password and secret; the old ones stop working at once), or `U` → reset in the dashboard | B |
| A worker's private key | `lazylongarm worker add <host> --reenroll` (revokes the old node, then the worker enrolls again with a new key and restarts; per the docs the old certificate is rejected within one heartbeat) | A, plus SSH to re-enroll |
| Another user's device or worker | `lazylongarm user remove <user>` revokes all of them (you cannot see them by name: admins only see counts) | A |
| Your laptop (admin certificate) lost or leaked | On the hub host, mint a new admin code (`longarm-hub join-code --role admin`), enroll another machine as the new admin, then `longarm admin revoke laptop` | **B**: no old admin certificate needed, so a lost laptop is recoverable |
| A join code | Expires after 15 minutes, works once, and still needs admin approval; `longarm admin join-codes` lists recent codes (without the codes themselves) | — |
| **The hub CA private key** | Worst case: no certificate can be trusted any more; create a new CA and re-enroll every machine (`hub setup`, then `worker add --reenroll` on each) | B, plus SSH to every machine |
| An SSH private key | Outside Longarm: replace the key and update `authorized_keys` on each machine; on Azure VMs `az vm user update` can reset it | Azure account |

So **SSH/sudo on the hub host plus the Azure account are the real top-level
privileges**; the admin certificate is a day-to-day convenience. Protect `~/.ssh`
and the Azure account (enable MFA), and run `lazylongarm hub backup` once,
keeping the backup encrypted and offline so the CA can be restored if the VM
dies.

## 8. Common questions

- **mTLS already encrypts everything; why SSH as well?** Different jobs: mTLS
  is the everyday channel between Longarm machines; SSH is lazylongarm's
  management channel to **install and repair** machines (Longarm itself has no
  remote installation).
- **Do Tailscale (WireGuard) and mTLS overlap?** A direct worker path over a
  tailnet has two layers of encryption (WireGuard outside, mTLS inside). The
  extra layer does no harm; Longarm does not depend on Tailscale and is just as
  safe over plain TCP.
- **Can the hub see my code or job output?** Not through the relay (end-to-end
  mTLS). It does know which machines and jobs exist (metadata), and whoever runs
  the hub can sign certificates, so trusting the hub's operator is still a
  premise.
- **Is a self-signed CA safe?** Safety does not depend on whether the CA is a
  public authority but on whether fingerprints are compared correctly and the CA
  key is protected. Public CAs suit strangers trusting each other; a private
  fleet is easier to control with its own CA.

## Further reading

- TOTP: [RFC 6238](https://www.rfc-editor.org/rfc/rfc6238); HOTP:
  [RFC 4226](https://www.rfc-editor.org/rfc/rfc4226)
- TLS 1.3: [RFC 8446](https://www.rfc-editor.org/rfc/rfc8446) (client
  certificates are what mTLS uses)
- Longarm: the enrollment and relay sections of the upstream `docs/design.md`,
  `docs/security.md` and `docs/usage.md`
