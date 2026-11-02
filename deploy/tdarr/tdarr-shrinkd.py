#!/usr/bin/env python3
"""Authenticated, serialized HTTP wrapper for the guarded host CLI."""

import hmac
import json
import os
import subprocess
import sys
import shlex
from http.server import BaseHTTPRequestHandler, HTTPServer
def load_config():
    try:
        with open("/etc/default/tdarr-shrink") as f:
            for line in f:
                parts = shlex.split(line, comments=True)
                if not parts:
                    continue
                if len(parts) != 1 or "=" not in parts[0]:
                    raise ValueError("expected KEY=value in configuration")
                key, value = parts[0].split("=", 1)
                os.environ[key] = value
    except FileNotFoundError:
        pass


load_config()
TOKEN_FILE = os.environ.get("TDARR_TOKEN_FILE", "/etc/tdarr-shrink.token")
PORT = int(os.environ.get("TDARR_SHRINKD_PORT", "8791"))
BIND = os.environ.get("TDARR_SHRINKD_BIND", "127.0.0.1")
roots = json.loads(os.environ.get("TDARR_ALLOWED_PREFIXES", '["/media/movies", "/media/series"]'))
if (not isinstance(roots, list) or not roots or any(
        not isinstance(p, str) or not p.startswith("/") or p == "/"
        or os.path.normpath(p) != p for p in roots)):
    sys.exit("tdarr-shrinkd: configure clean absolute allowed roots")
PREFIXES = tuple(p + "/" for p in roots)
SCRIPT = "/usr/local/bin/tdarr-shrink"
MAX_PATHS = 16
MAX_PATH_LEN = 512
# tdarr-shrink arms a worker and waits up to ~40s for worker activity before
# returning; give it generous headroom.
RUN_TIMEOUT = 300


def read_token():
    with open(TOKEN_FILE) as f:
        return f.read().strip()


class Handler(BaseHTTPRequestHandler):
    server_version = "tdarr-shrinkd/1.0"

    def _reply(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path not in ("/shrink", "/validate"):
            self._reply(404, {"error": "not found"})
            return
        auth = self.headers.get("Authorization", "")
        try:
            token = read_token()
        except OSError:
            token = ""
        if not token:
            self._reply(503, {"error": "authentication unavailable"})
            return
        expected = "Bearer " + token
        if not hmac.compare_digest(auth.encode(), expected.encode()):
            self._reply(403, {"error": "bad token"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length < 0 or length > 64 * 1024:
                self._reply(413, {"error": "body too large"})
                return
            req = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._reply(400, {"error": "bad JSON"})
            return

        if not isinstance(req, dict):
            self._reply(400, {"error": "bad JSON"})
            return
        paths = req.get("paths")
        if not isinstance(paths, list) or not paths:
            self._reply(400, {"error": "paths must be a non-empty array of strings"})
            return
        if len(paths) > MAX_PATHS:
            self._reply(400, {"error": f"at most {MAX_PATHS} paths per batch"})
            return
        for p in paths:
            if (not isinstance(p, str) or len(p) > MAX_PATH_LEN
                    or any(c in p for c in "\x00\n\r")
                    or not p.startswith(PREFIXES)):
                self._reply(400, {"error": "each path must be a non-empty string"})
                return
        with_pc = req.get("withPc", False)
        if not isinstance(with_pc, bool):
            self._reply(400, {"error": "withPc must be a boolean"})
            return

        cmd = ["sudo", "-n", SCRIPT]
        if self.path == "/validate":
            cmd.append("--validate-only")
        if with_pc:
            cmd.append("--with-pc")
        cmd.extend(paths)
        try:
            proc = subprocess.run(
                cmd,
                capture_output=True, text=True, timeout=RUN_TIMEOUT,
            )
        except subprocess.TimeoutExpired:
            self._reply(504, {"error": "tdarr-shrink timed out after %ds" % RUN_TIMEOUT})
            return
        except OSError as e:
            self._reply(500, {"error": f"could not run {SCRIPT}: {e}"})
            return

        if self.path == "/validate":
            if proc.returncode != 0:
                self._reply(503, {"error": "media safety validation failed; batch refused"})
                return
            try:
                validated = json.loads(proc.stdout)
                files = validated["paths"]
                if (not isinstance(files, list) or not 0 < len(files) <= MAX_PATHS
                        or any(not isinstance(p, str) or not p.startswith(PREFIXES) for p in files)):
                    raise ValueError("invalid validation result")
            except (ValueError, KeyError, TypeError):
                self._reply(503, {"error": "media validation unavailable; batch refused"})
                return
            self._reply(200, {"paths": files})
            return

        self._reply(200, {
            "exit_code": proc.returncode,
            "stdout": proc.stdout,
            "stderr": proc.stderr,
        })

    def log_message(self, fmt, *args):
        sys.stderr.write("tdarr-shrinkd: %s\n" % (fmt % args))


def main():
    token = read_token()
    if not token:
        sys.exit(f"tdarr-shrinkd: {TOKEN_FILE} is empty")
    # HTTPServer is single-threaded: batches are serialized, which is what
    # global queue state wants.
    httpd = HTTPServer((BIND, PORT), Handler)
    print(f"tdarr-shrinkd: listening on {BIND}:{PORT}", flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
