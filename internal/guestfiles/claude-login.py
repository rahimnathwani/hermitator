# Runs `claude setup-token` without a person at its terminal, and reports
# progress as JSON lines on stdout:
#
#   {"type":"url","url":"https://claude.com/cai/oauth/authorize?..."}
#   {"type":"token","token":"sk-ant-oat01-..."}
#   {"type":"error","message":"..."}
#
# setup-token is an interactive program: it prints a sign-in URL, then
# waits for the code the sign-in page shows to be pasted in. So it runs in
# a pseudo-terminal wide enough that the URL is never wrapped, and the code
# arrives as a file the host writes (argv[1]), which this types in. Driving
# Claude Code's own command means hermitator keeps no copy of Anthropic's
# OAuth flow.
#
# It runs with a throwaway HOME, so nothing it writes lands in the agent's
# ~/.claude, and without the agent's token in its environment.

import json
import os
import pty
import re
import select
import shutil
import signal
import struct
import sys
import tempfile
import time

try:
    import fcntl
    import termios
except ImportError:  # not on a Unix; nothing below would work anyway
    fcntl = termios = None

URL = re.compile(r"https://\S+/oauth/authorize\?\S+")
TOKEN = re.compile(r"sk-ant-oat01-[A-Za-z0-9_-]{20,}")
# What setup-token says when the code is wrong or stale.
REFUSED = re.compile(r"OAuth\s*error|Press\s*Enter\s*to\s*retry", re.I)
# Escape sequences an Ink app draws with. Spaces are often drawn as cursor
# moves, so text comes out squashed; that is fine for what is matched here.
ANSI = re.compile(r"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78]")

WAIT_FOR_URL = 60
WAIT_FOR_CODE = 15 * 60
WAIT_FOR_TOKEN = 60


def out(event):
    sys.stdout.write(json.dumps(event) + "\n")
    sys.stdout.flush()


def main():
    if len(sys.argv) != 2:
        out({"type": "error", "message": "usage: claude-login.py CODE_FILE"})
        return 2
    code_file = sys.argv[1]
    claude = os.environ.get("HERMITATOR_CLAUDE_BIN") or shutil.which("claude")
    if not claude:
        out({"type": "error", "message": "Claude Code is not installed in this sandbox"})
        return 1

    home = tempfile.mkdtemp(prefix="claude-login-")
    env = {k: v for k, v in os.environ.items() if k not in ("CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")}
    env.update({"HOME": home, "TERM": "xterm-256color", "COLUMNS": "1000", "LINES": "50", "BROWSER": "true"})

    pid, fd = pty.fork()
    if pid == 0:
        os.execve(claude, [claude, "setup-token"], env)
    if fcntl:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 50, 1000, 0, 0))

    def stop(*_):
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        shutil.rmtree(home, ignore_errors=True)
        sys.exit(1)

    signal.signal(signal.SIGTERM, stop)

    text = ""
    url = None
    sent_code_at = None
    started = time.time()
    try:
        while True:
            now = time.time()
            if url is None and now - started > WAIT_FOR_URL:
                return fail("setup-token showed no sign-in link", text)
            if url is not None and sent_code_at is None and now - started > WAIT_FOR_CODE:
                return fail("no code arrived in time; start again", "")
            if sent_code_at is not None and now - sent_code_at > WAIT_FOR_TOKEN:
                return fail("setup-token did not accept the code", text)

            ready, _, _ = select.select([fd], [], [], 0.2)
            if ready:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    chunk = b""
                if not chunk:
                    m = TOKEN.search(text)
                    if m:
                        out({"type": "token", "token": m.group(0)})
                        return 0
                    return fail("setup-token ended", text)
                text += ANSI.sub("", chunk.decode("utf-8", "replace"))
                # Keep the tail; the URL and the token are near the end.
                text = text[-20000:]

            m = TOKEN.search(text)
            if m:
                out({"type": "token", "token": m.group(0)})
                return 0
            if sent_code_at is not None and REFUSED.search(text) and now - sent_code_at > 0.5:
                return fail("the code was not accepted; codes work once, so start again", text)
            if url is None:
                m = URL.search(text)
                if m:
                    url = m.group(0)
                    out({"type": "url", "url": url})
                    # Anything after this is about the code.
                    text = text[m.end():]
            elif sent_code_at is None and os.path.exists(code_file):
                with open(code_file) as f:
                    code = f.read().strip()
                os.remove(code_file)
                if code:
                    # Claude Code takes a burst of characters as a paste and
                    # ignores an Enter inside one, so a code of real length
                    # typed with its Enter in one write is never submitted.
                    # Enter goes on its own once the paste has landed.
                    os.write(fd, code.encode())
                    time.sleep(0.5)
                    os.write(fd, b"\r")
                    sent_code_at = time.time()
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        shutil.rmtree(home, ignore_errors=True)


def fail(message, text):
    # The last few lines say why, minus anything that looks like a token.
    lines = [l.strip() for l in TOKEN.sub("[token]", text).splitlines() if l.strip()]
    detail = " / ".join(lines[-3:])[:300]
    out({"type": "error", "message": message + (": " + detail if detail else "")})
    return 1


if __name__ == "__main__":
    sys.exit(main())
