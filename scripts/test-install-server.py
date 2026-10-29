#!/usr/bin/env python3
"""Build real release assets; check a disposable server install without root."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def main():
    env = os.environ.copy()
    for key in ("API_TOKEN", "AGENT_TOKEN", "OPERATOR_TOKEN", "PUBLIC_URL", "INSTALL_BASE_URL"):
        env.pop(key, None)
    env.update(APPRISE_URL="", DIGEST_APPRISE_URL="", ACTIONS_ENABLED="0", ICONS_FETCH="0")

    def run(*args, success=True):
        result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True, timeout=300)
        if success and result.returncode:
            # Do not print environment, server logs, or response bodies.
            raise RuntimeError("local installer check failed: " + result.stderr.decode()[:500])
        if not success:
            assert result.returncode != 0
        return result

    run("make", "release-artifacts", "VERSION=v0.0.0-local")
    with tempfile.TemporaryDirectory(prefix="inventory-server-") as tmp:
        tmp = Path(tmp)
        prefix = tmp / "root"
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        installer = str(ROOT / "scripts/get.sh")
        args = ["sh", installer, "--from", str(ROOT / "dist"), "--prefix", str(prefix),
                "--no-docker", "--port", str(port)]
        run(*args)
        run(*args)  # Same manifest resolves latest; reruns upgrade in place.
        binary = prefix / "usr/local/bin/homelab-inventory"
        agents = prefix / "usr/share/homelab-inventory/agent"
        state = prefix / "var/lib/homelab-inventory"
        unit = prefix / "etc/systemd/system/homelab-inventory.service"
        assert binary.is_file() and os.access(binary, os.X_OK)
        assert state.stat().st_mode & 0o777 == 0o700
        assert "ProtectSystem=strict" in unit.read_text()
        assert f"Environment=AGENT_BIN_DIR={agents}" in unit.read_text()
        for line in (agents / "SHA256SUMS").read_text().splitlines():
            expected, path = line.split()
            assert hashlib.sha256((agents / path).read_bytes()).hexdigest() == expected
        original = hashlib.sha256(binary.read_bytes()).hexdigest()
        corrupted = tmp / "corrupted"
        shutil.copytree(ROOT / "dist", corrupted)
        for archive in corrupted.glob("*.tar.gz"):
            with archive.open("ab") as file:
                file.write(b"tampered")
        bad_args = ["sh", installer, "--from", str(corrupted), "--prefix", str(prefix), "--no-docker"]
        refusal = run(*bad_args, success=False)
        assert b"checksum mismatch" in refusal.stderr
        assert hashlib.sha256(binary.read_bytes()).hexdigest() == original
        fresh = tmp / "untouched"
        run("sh", installer, "--from", str(corrupted), "--prefix", str(fresh), "--no-docker", success=False)
        assert not fresh.exists()

        # Use the installed server and packaged enrollment assets, never bin/api.
        env["AGENT_BIN_DIR"] = str(agents)
        origin = f"http://127.0.0.1:{port}"
        with (tmp / "server.log").open("wb") as logfile:
            proc = subprocess.Popen([str(binary), "-addr", f"127.0.0.1:{port}", "-dir", str(state)],
                                    env=env, stdout=logfile, stderr=logfile)
            try:
                for _ in range(100):
                    if proc.poll() is not None:
                        raise RuntimeError("installed server exited")
                    try:
                        urllib.request.urlopen(origin + "/healthz", timeout=1).close()
                        break
                    except (urllib.error.URLError, ConnectionError):
                        time.sleep(0.05)
                else:
                    raise RuntimeError("installed server did not become healthy")
                with urllib.request.urlopen(origin + "/setup/status", timeout=5) as response:
                    status = json.load(response)
                    assert status["state"] == "unclaimed" and 0 < status["window_seconds_left"] <= 600
                with urllib.request.urlopen(origin + "/install.sh", timeout=5) as response:
                    assert response.read().startswith(b"#!/bin/sh\n")
                for target in ("linux-amd64", "linux-arm64"):
                    for name in ("discover", "agentupdate"):
                        with urllib.request.urlopen(origin + f"/agent/bin/{target}/{name}", timeout=5) as response:
                            assert hashlib.sha256(response.read()).hexdigest() == hashlib.sha256((agents / target / name).read_bytes()).hexdigest()
            finally:
                proc.terminate()
                proc.wait(timeout=10)
        retained = state / "keep"
        retained.write_text("persistent inventory")
        run("sh", installer, "--prefix", str(prefix), "--uninstall")
        assert retained.exists() and not binary.exists() and not agents.exists() and not unit.exists()
        run(*args)
        run("sh", installer, "--prefix", str(prefix), "--uninstall", "--purge", "--yes")
        assert not state.exists()
    print("local release + server layout/enrollment/checksum refusal/upgrade/uninstall: PASS")


if __name__ == "__main__":
    # Match make's documented file permissions even when the parent has umask 077.
    os.umask(0o022)
    main()
