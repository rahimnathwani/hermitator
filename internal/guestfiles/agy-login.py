# Runs `agy acp --login` (Antigravity's Google sign-in) without a person at
# its terminal, and reports progress as JSON lines on stdout:
#
#   {"type":"url","url":"https://accounts.google.com/o/oauth2/auth?..."}
#   {"type":"token","token":"{\"token\":{...},\"auth_method\":...}"}
#   {"type":"error","message":"..."}
#
# agy prints a Google sign-in link and waits for the authorization code
# the page it lands on shows (or that page's whole URL) to be pasted in.
# So it runs in a pseudo-terminal wide enough that the link is never
# wrapped, and the code arrives as a file the host writes (argv[1]), which
# this types in. When agy exits, the token file it wrote is the token:
# hermitator hands that file back to agy on every turn.
#
# It runs with a throwaway HOME, so nothing it writes lands in the agent's
# own ~/.gemini.

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

URL = re.compile(r"https://accounts\.google\.com/o/oauth2/\S+")
TOKEN_FILE = os.path.join(".gemini", "antigravity-cli", "antigravity-oauth-token")
ANSI = re.compile(r"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78]")
# Long strings that could be codes or tokens, kept out of error messages.
SECRET = re.compile(r"[A-Za-z0-9/_\-.]{24,}")

WAIT_FOR_URL = 60
WAIT_FOR_CODE = 15 * 60
WAIT_FOR_TOKEN = 60


def out(event):
    sys.stdout.write(json.dumps(event) + "\n")
    sys.stdout.flush()


def main():
    if len(sys.argv) != 2:
        out({"type": "error", "message": "usage: agy-login.py CODE_FILE"})
        return 2
    code_file = sys.argv[1]
    agy = os.environ.get("HERMITATOR_AGY_BIN") or shutil.which("agy")
    if not agy:
        out({"type": "error", "message": "Antigravity is not installed in this sandbox"})
        return 1

    home = tempfile.mkdtemp(prefix="agy-login-")
    env = dict(os.environ)
    env.update({"HOME": home, "TERM": "xterm-256color", "COLUMNS": "1000", "LINES": "50", "BROWSER": "true",
                "AGY_CLI_DISABLE_AUTO_UPDATE": "1"})

    pid, fd = pty.fork()
    if pid == 0:
        os.execve(agy, [agy, "acp", "--login", "--disable-telemetry"], env)
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
                return fail("agy showed no sign-in link", text)
            if url is not None and sent_code_at is None and now - started > WAIT_FOR_CODE:
                return fail("no code arrived in time; start again", "")
            if sent_code_at is not None and now - sent_code_at > WAIT_FOR_TOKEN:
                return fail("agy did not accept the code", text)

            ready, _, _ = select.select([fd], [], [], 0.2)
            if ready:
                try:
                    chunk = os.read(fd, 65536)
                except OSError:
                    chunk = b""
                if not chunk:
                    break
                text += ANSI.sub("", chunk.decode("utf-8", "replace"))
                text = text[-20000:]

            if url is None:
                m = URL.search(text)
                if m:
                    url = m.group(0)
                    out({"type": "url", "url": url})
                    text = text[m.end():]
            elif sent_code_at is None and os.path.exists(code_file):
                with open(code_file) as f:
                    code = f.read().strip()
                os.remove(code_file)
                if code:
                    os.write(fd, code.encode())
                    time.sleep(0.5)
                    os.write(fd, b"\r")
                    sent_code_at = time.time()

        if sent_code_at is None:
            return fail("agy ended before a code arrived", text)
        _, status = os.waitpid(pid, 0)
        try:
            with open(os.path.join(home, TOKEN_FILE)) as f:
                token = f.read()
            json.loads(token)
        except (OSError, ValueError):
            return fail("the code was not accepted; codes work once, so start again", text)
        out({"type": "token", "token": token})
        return 0
    finally:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        shutil.rmtree(home, ignore_errors=True)


def fail(message, text):
    lines = [l.strip() for l in SECRET.sub("[…]", text).splitlines() if l.strip()]
    detail = " / ".join(lines[-3:])[:300]
    out({"type": "error", "message": message + (": " + detail if detail else "")})
    return 1


if __name__ == "__main__":
    sys.exit(main())
