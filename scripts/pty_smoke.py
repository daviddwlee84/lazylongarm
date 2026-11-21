# /// script
# requires-python = ">=3.10"
# dependencies = ["pyte==0.8.2"]
# ///
"""Drive bin/lazylongarm in a real PTY with fake `ssh` and `longarm`.

Nothing real is contacted except (optionally) the GitHub API for the latest
release tag. HOME, XDG dirs and PATH are isolated; the fakes log their argv so
the script can assert that canceled wizards changed nothing.

  go build -o bin/lazylongarm ./cmd/lazylongarm && uv run scripts/pty_smoke.py
"""
import codecs
import fcntl
import os
import re
from pathlib import Path
import pty
import select
import struct
import subprocess
import sys
import tempfile
import termios
import time

sys.path.insert(0, str(Path(__file__).parent))
from pty_emulator import Screen, Stream  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
FIXTURES = ROOT / "internal/longarm/testdata/upstream"

FAKE_SSH = r"""#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
case " $* " in
  *" -G "*) printf 'hostname fakehub.example.com\ncontrolmaster false\ncontrolpersist no\n'; exit 0;;
esac
for last; do :; done
case "$last" in
  *probe_done*) [ -f "$FAKE_SLOW" ] && sleep 20; cat "$FAKE_PROBE";;
  *"systemctl is-active longarm-hub"*) printf 'active\n0.1.42\n{"ok":true,"version":"v0.1.42"}\n';;
esac
exit 0
"""

FAKE_LONGARM = r"""#!/bin/sh
printf 'longarm %s\n' "$*" >> "$FAKE_LOG"
F="$FIXTURES"
case "$1 $2" in
  "version --json") cat "$F/version.json";;
  "whoami --json") cat "$F/whoami.json";;
  "worker ls") cat "$F/worker-ls.json";;
  "jobs --json") cat "$F/jobs.json";;
  "doctor --json") cat "$F/doctor.json";;
  "admin user")
    case "$3" in
      ls) printf '{"users":[{"name":"alice","device_count":1,"worker_count":1}],"tenancy":true}\n';;
      *) echo "unexpected longarm $*" >&2; exit 2;;
    esac;;
  logs*) printf 'hello from the job\nsecond line\n';;
  *) echo "unexpected longarm $*" >&2; exit 2;;
esac
"""

# Stand-ins for the local clipboard tools, so copying never touches the real
# clipboard and the test can read what was copied.
FAKE_CLIP = r"""#!/bin/sh
cat > "$FAKE_CLIP"
"""

FAKE_HC = r"""#!/bin/sh
printf 'hc %s\n' "$*" >> "$FAKE_LOG"
case "$1 $2" in
  "machine ls") printf '{"machines":[]}\n';;
  *) exit 0;;
esac
"""

PROBE = """os=Linux
arch=x86_64
distro_id=ubuntu
distro_codename=jammy
distro_pretty=Ubuntu 22.04.2 LTS
user=david
home=/home/david
sudo=nopasswd
has_systemctl=yes
has_apt-get=yes
has_curl=yes
has_gpg=yes
mem_total_kb=1893040
mem_avail_kb=1109092
cpus=1
listen=tcp 0.0.0.0:22
probe_done=yes
"""

CONFIG = """default_hub = "fakehub"
[[hubs]]
  id = "fakehub"
  mode = "public"
  address = "fakehub.example.com:7443"
  ca_fingerprint = "sha256:abcd"
  ssh_host = "fakehub"
  panel = "127.0.0.1:7480"
[[targets]]
  id = "worker-1"
  name = "worker-1"
  ssh_host = "box2"
  hub = "fakehub"
  worker_name = "worker-1"
"""


class Terminal:
    def __init__(self, args, env, cols=120, rows=32):
        self.master, slave = pty.openpty()
        self.screen = Screen(cols, rows)
        self.stream = Stream(self.screen)
        self.decoder = codecs.getincrementaldecoder("utf-8")("replace")
        self.raw = b""  # everything the program wrote, for escape-sequence checks
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))

        def child():
            os.setsid()
            fcntl.ioctl(slave, termios.TIOCSCTTY, 0)

        self.proc = subprocess.Popen([str(ROOT / "bin/lazylongarm"), *args], stdin=slave, stdout=slave,
                                     stderr=slave, env=env, preexec_fn=child, close_fds=True)
        os.close(slave)

    def read(self, seconds=0.2):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            ready, _, _ = select.select([self.master], [], [], max(0, deadline - time.monotonic()))
            if not ready:
                break
            try:
                data = os.read(self.master, 65536)
            except OSError:
                break
            if not data:
                break
            self.raw += data
            if b"\x1b[6n" in data:
                os.write(self.master, b"\x1b[1;1R")
            self.stream.feed(self.decoder.decode(data))

    def send(self, data):
        os.write(self.master, data.encode() if isinstance(data, str) else data)
        self.read()

    def find(self, needle):
        """Zero-based (x, y) of needle on screen."""
        for y, line in enumerate(self.screen.display):
            x = line.find(needle)
            if x >= 0:
                return x, y
        raise AssertionError(f"{needle!r} not on screen\n{self.text()}")

    def click(self, x, y):
        """Left click at zero-based (x, y) as SGR mouse reports (one-based)."""
        self.send(f"\x1b[<0;{x + 1};{y + 1}M")
        self.send(f"\x1b[<0;{x + 1};{y + 1}m")

    def text(self):
        return "\n".join(line.rstrip() for line in self.screen.display)

    def wait(self, needle, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.read(0.2)
            if needle in self.text():
                return
        raise AssertionError(f"timed out waiting for {needle!r}\n----\n{self.text()}")

    def exit_code(self, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.read(0.1)
            code = self.proc.poll()
            if code is not None:
                return code
        self.proc.kill()
        raise AssertionError("process did not exit\n" + self.text())


def environment(root):
    fake = root / "bin"
    fake.mkdir()
    for name, body in (("ssh", FAKE_SSH), ("longarm", FAKE_LONGARM), ("hc", FAKE_HC), ("pbcopy", FAKE_CLIP), ("xclip", FAKE_CLIP)):
        p = fake / name
        p.write_text(body)
        p.chmod(0o755)
    (root / "probe.txt").write_text(PROBE)
    home = root / "home"
    (home / ".ssh").mkdir(parents=True)
    (home / ".ssh/config").write_text("Host fakehub\n  HostName fakehub.example.com\nHost box2\n")
    cfg = root / "xdg-config/lazylongarm"
    cfg.mkdir(parents=True)
    (cfg / "config.toml").write_text(CONFIG)
    return {
        "PATH": f"{fake}:/usr/bin:/bin",
        "HOME": str(home),
        "XDG_CONFIG_HOME": str(root / "xdg-config"),
        "XDG_STATE_HOME": str(root / "xdg-state"),
        "XDG_CACHE_HOME": str(root / "xdg-cache"),
        "FAKE_LOG": str(root / "fake.log"),
        "FAKE_PROBE": str(root / "probe.txt"),
        "FAKE_SLOW": str(root / "slow"),
        "FAKE_CLIP": str(root / "clipboard.txt"),
        "FIXTURES": str(FIXTURES),
        "TERM": "xterm-256color",
        "LANG": "en_US.UTF-8",
    }


def dashboard(env):
    t = Terminal([], env)
    t.wait("hub fakehub")
    t.wait("worker-1")
    assert "Workers" in t.text() and "Details" in t.text(), t.text()
    t.send("2")
    t.wait("Jobs")
    t.wait("running")
    t.wait("hello from the job")  # log tail of the selected job
    t.send("K")
    t.wait("Kill job")
    t.send("q")  # q cancels a confirmation; it must not quit
    t.wait("canceled")
    assert t.proc.poll() is None, "q in a confirmation quit the app"
    t.send("?")
    t.wait("follow log")
    t.send("\x1b")
    t.send("3")
    t.wait("longarm-hub.service active")
    t.wait("longarm doctor")
    t.send("q")
    code = t.exit_code()
    assert code == 0, f"dashboard exit {code}"
    print("dashboard: ok")


def wizard_cancel(env, log):
    log.write_text("")
    t = Terminal(["hub", "setup", "--interactive", "--host", "fakehub"], env)
    t.wait("Hub on fakehub")
    assert "fakehub.example.com" in t.text(), t.text()
    t.send("\x13")  # Ctrl+S: Review from any field
    t.wait("Review")
    assert "[ Back ]" in t.text(), "Review must default to Back"
    for _ in range(12):
        t.send("j")  # scroll the plan
    t.wait("public_addr")
    t.send("\x1b")  # Back to the form, draft kept
    t.wait("Hub on fakehub")
    t.send("\x1b")  # Back to the host picker
    t.wait("SSH host")
    assert "box2" in t.text(), t.text()
    t.send("\x03")
    code = t.exit_code()
    assert code == 130, f"canceled wizard exit {code}\n{t.text()}"
    calls = log.read_text()
    for marker in ("longarm.gpg", "install -d -m 0700", "bootstrap --admin-code-ttl", "systemctl enable --now", "longarm join", "worker add"):
        assert marker not in calls, f"canceled wizard ran an apply step ({marker})"
    print("hub setup wizard (review/back/cancel, nothing applied): ok")


def ctrl_c_in_handoff(env, root):
    """Ctrl+C during a terminal handoff cancels that command, not the dashboard."""
    (root / "slow").write_text("1")
    try:
        t = Terminal([], env)
        t.wait("worker-1")
        t.send("3")
        t.send("S")  # hub setup wizard, handed the terminal
        t.wait("Where should longarm-hub run?")
        t.send("\x13")  # Ctrl+S: probe the first suggested host
        t.wait("Probing")
        t.send("\x03")  # SIGINT while the probe runs (cooked mode)
        t.wait("hub fakehub", timeout=10)
        assert t.proc.poll() is None, "Ctrl+C in a handoff killed the dashboard\n" + t.text()
        t.send("q")
        assert t.exit_code() == 0
        print("Ctrl+C in a handoff returns to the dashboard: ok")
    finally:
        (root / "slow").unlink(missing_ok=True)


def companion_remotes_cancel(env, log):
    """The remotes picker preselects unregistered machines; Review shows the
    hc command; canceling there registers nothing."""
    log.write_text("")
    t = Terminal(["companion", "remotes", "--tool", "hc"], env)
    t.wait("Register which machines in hc?")
    assert "worker-1" in t.text(), t.text()
    t.send("\r")
    t.wait("Review")
    t.wait("hc machine add box2 --install --name worker-1")
    t.send("\x03")
    code = t.exit_code()
    assert code == 130, f"canceled review exit {code}\n{t.text()}"
    assert "hc machine add" not in log.read_text(), "canceled review registered a machine"
    print("companion remotes (picker/review/cancel, nothing applied): ok")


def panel_access_cancel(env, log):
    """panel-access: the wizard opens; the tailnet Review shows tailscale
    serve; canceling there installs and logs in nothing."""
    log.write_text("")
    t = Terminal(["hub", "panel-access"], env)
    t.wait("Web panel access")
    assert "SSH tunnel only" in t.text(), t.text()
    t.send("\x03")
    assert t.exit_code() == 130
    t = Terminal(["hub", "panel-access", "tailnet"], env)
    t.wait("Review")
    for _ in range(30):
        t.send("j")  # scroll the plan
    t.wait("tailscale serve --bg --https=443")
    t.send("\x03")
    code = t.exit_code()
    assert code == 130, f"canceled review exit {code}\n{t.text()}"
    calls = log.read_text()
    for marker in ("noarmor.gpg", "apt-get install", "tailscale up", "tailscale serve --bg"):
        assert marker not in calls, f"canceled panel-access ran an apply step ({marker})"
    print("hub panel-access (wizard/review/cancel, nothing applied): ok")


def user_invite_cancel(env, log):
    """user invite refuses while a fleet worker exists; with
    --allow-fleet-workers it asks first, and canceling adds nobody."""
    log.write_text("")
    t = Terminal(["user", "invite", "bob"], env)
    code = t.exit_code()
    assert code == 2, f"invite with a fleet worker exit {code}\n{t.text()}"
    assert "worker-2" in t.text(), t.text()
    t = Terminal(["user", "invite", "bob", "--allow-fleet-workers"], env)
    t.wait("Invite bob")
    t.wait("client join code")
    t.send("\x03")
    code = t.exit_code()
    assert code == 130, f"canceled invite exit {code}\n{t.text()}"
    assert "admin user add" not in log.read_text(), "canceled invite added the user"
    print("user invite (fleet-worker gate, confirm, cancel adds nobody): ok")


def mouse(env):
    """Mouse capture is on by default: clicks switch tabs and fire footer
    actions on release; M turns capture off and back on."""
    t = Terminal([], env)
    t.wait("worker-1")
    assert b"\x1b[?1002h" in t.raw and b"\x1b[?1006h" in t.raw, "cell-motion SGR mouse not enabled"
    t.click(*t.find("2 Jobs"))
    t.wait("Job · log tail")
    t.wait("hello from the job")
    # Drag over the two log lines: they are copied whole (OSC 52 + local).
    x, y = t.find("hello from the job")
    t.send(f"\x1b[<0;{x + 1};{y + 1}M")
    t.send(f"\x1b[<32;{x + 3};{y + 2}M")  # motion with the left button held
    t.send(f"\x1b[<0;{x + 3};{y + 2}m")
    t.wait("copied 2 lines")
    assert b"\x1b]52;c;" in t.raw, "no OSC 52 clipboard write"
    clip = Path(env["FAKE_CLIP"])
    deadline = time.monotonic() + 5
    while not clip.exists() and time.monotonic() < deadline:
        t.read(0.1)
    assert clip.read_text() == "hello from the job\nsecond line", repr(clip.read_text())
    x, y = t.find("? help")
    t.click(x, y)
    t.wait("Help · Jobs")
    t.click(0, 0)  # outside the modal: closes it, nothing behind reacts
    t.wait("Job · log tail")
    assert "Help · Jobs" not in t.text(), t.text()
    t.click(*t.find("1 Fleet"))
    t.wait("Workers")
    before = len(t.raw)
    t.send("M")
    t.wait("mouse off")
    assert b"\x1b[?1002l" in t.raw[before:], "M did not release the mouse"
    t.send("M")
    t.wait("mouse on")
    t.send("q")
    assert t.exit_code() == 0
    print("mouse (tabs, drag-to-copy, footer help, modal outside click, M toggle): ok")


def no_color(env):
    """NO_COLOR keeps the layout and its markers but emits no colors."""
    t = Terminal([], {**env, "NO_COLOR": "1"})
    t.wait("worker-1")
    assert "[1 Fleet" in t.text(), t.text()
    colors = re.findall(rb"\x1b\[[0-9;]*(?:3[0-7]|9[0-7]|4[0-7]|10[0-7]|38;5|48;5|38;2|48;2)[0-9;]*m", t.raw)
    assert not colors, f"NO_COLOR dashboard emitted colors: {colors[:5]}"
    t.send("q")
    assert t.exit_code() == 0
    print("NO_COLOR dashboard (markers kept, no colors): ok")


def narrow(env):
    t = Terminal([], env, cols=64, rows=18)
    t.wait("worker-1")
    last = t.screen.display[-1]
    assert "q quit" in last, repr(last)
    t.send("q")
    assert t.exit_code() == 0
    print("narrow 64x18 dashboard: ok")


def main():
    if not (ROOT / "bin/lazylongarm").exists():
        sys.exit("build first: go build -o bin/lazylongarm ./cmd/lazylongarm")
    with tempfile.TemporaryDirectory(prefix="lazylongarm-pty-") as scratch:
        root = Path(scratch)
        env = environment(root)
        dashboard(env)
        wizard_cancel(env, root / "fake.log")
        ctrl_c_in_handoff(env, root)
        companion_remotes_cancel(env, root / "fake.log")
        panel_access_cancel(env, root / "fake.log")
        user_invite_cancel(env, root / "fake.log")
        mouse(env)
        no_color(env)
        narrow(env)


if __name__ == "__main__":
    main()
