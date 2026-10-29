#!/usr/bin/env python3
"""Disposable installer/poster/update integration check; no root or systemd."""
import json
import os
from pathlib import Path
import platform
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent.parent


def main():
    arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
    with tempfile.TemporaryDirectory(prefix="inventory-install-") as tmp:
        tmp = Path(tmp)
        prefix = tmp / "root"
        artifacts = tmp / "artifacts"
        artifacts.mkdir()
        # Loopback is the only network address used by this test.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            port = sock.getsockname()[1]
        origin = f"http://127.0.0.1:{port}"
        token = secrets.token_hex(32)
        agent_token = secrets.token_hex(32)
        token_file = tmp / "token"
        token_file.write_text(agent_token + "\n")
        token_file.chmod(0o600)
        release_tree = tmp / "releases"
        key = tmp / "signer.key"
        env = os.environ.copy()
        env.update(API_TOKEN=token, AGENT_TOKEN=agent_token, AGENT_RELEASE_DIR=str(release_tree),
                   AGENT_CANARY_HOSTS=socket.gethostname(), AGENT_PLATFORMS="linux/amd64,linux/arm64",
                   APPRISE_URL="", DIGEST_APPRISE_URL="", OPERATOR_TOKEN="", ACTIONS_ENABLED="0")
        raw = {"schema_version": "5", "collected_at": "__TIME__",
               "host": {"hostname": socket.gethostname()}, "tool": {"version": "local-test", "revision": 2},
               "docker": None, "diagnostics": [{"severity": "warning", "message": "synthetic partial inventory"}]}
        payload = json.dumps(raw)
        collector_script = ("#!/bin/sh\nif [ \"${1:-}\" = -list ]; then exit 0; fi\nprintf '%s' '" + payload
                            + "' | sed \"s/__TIME__/$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)/\"\n")
        for target in ("amd64", "arm64"):
            binary = artifacts / f"discover-linux-{target}"
            binary.write_text(collector_script)
            binary.chmod(0o755)
        shutil.copy2(ROOT / "bin" / f"agentupdate-linux-{arch}", artifacts)

        def run(*args, **kwargs):
            result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True, timeout=60, **kwargs)
            if result.returncode:
                # Only safe command outcomes are printed; never print argv/env/token.
                raise RuntimeError("local integration command failed: " + result.stderr.decode()[:500])
            return result

        signer = str(ROOT / "bin" / "agentsign")
        env.update(SIGNER_HOST="", SIGNER_BIN=signer, SIGNER_KEY=str(key),
                   SIGNER_DIR=str(release_tree), SIGNER_INPUT=str(artifacts))
        release_script = str(ROOT / "scripts/release-agent.sh")
        public = tmp / "public.key"
        public.write_bytes(run(release_script, "keygen").stdout)
        run(release_script, "release", "local-test")
        with (tmp / "api.log").open("wb") as logfile:
            proc = subprocess.Popen([str(ROOT / "bin" / "api"), "-addr", f"127.0.0.1:{port}", "-dir", str(tmp / "data")],
                                    cwd=ROOT, env=env, stdout=logfile, stderr=logfile)
            try:
                for _ in range(100):
                    if proc.poll() is not None:
                        raise RuntimeError("temporary API exited")
                    try:
                        urllib.request.urlopen(origin + "/healthz", timeout=1).close()
                        break
                    except (urllib.error.URLError, ConnectionError):
                        time.sleep(0.05)
                else:
                    raise RuntimeError("temporary API did not become healthy")
                installer_copy = tmp / "install-agent.sh"
                shutil.copy2(ROOT / "scripts" / "install-agent.sh", installer_copy)
                installer = str(installer_copy)
                args = [installer, "--prefix", str(prefix), "--api-url", origin, "--token-file", str(token_file), "--binary-dir", str(artifacts)]
                run(*args)
                run(*args)  # idempotent plain installation, first report each time
                state = prefix / "var/lib/homelab-agent"
                assert (state / "token").stat().st_mode & 0o777 == 0o600
                assert (state / "config.json").stat().st_mode & 0o777 == 0o600
                assert not list(state.glob(".inventory-*"))
                def latest():
                    request = urllib.request.Request(origin + "/hosts/" + socket.gethostname(), headers={"Authorization": "Bearer " + token})
                    with urllib.request.urlopen(request, timeout=5) as response:
                        return json.load(response)
                # The installed poster just completed POST /inventory with AGENT_TOKEN.
                # That same credential must not reach any dashboard/admin data.
                for method, path in (("GET", "/hosts"), ("PUT", "/settings")):
                    request = urllib.request.Request(origin + path, method=method,
                        data=b"{}" if method == "PUT" else None,
                        headers={"Authorization": "Bearer " + agent_token, "Content-Type": "application/json"})
                    try:
                        urllib.request.urlopen(request, timeout=5).close()
                    except urllib.error.HTTPError as error:
                        assert error.code in (401, 403)
                    else:
                        raise AssertionError("agent credential accepted on admin route")
                first = latest()
                assert first.get("docker") is None and first["diagnostics"][0]["message"] == raw["diagnostics"][0]["message"]
                assert first.get("agent_update") is None
                # Existing schedule/runner gains signed mode through local enrollment.
                updater = str(state / "agentupdate")
                run(updater, "-dir", str(state), "-public", str(public), "-initialize", str(release_tree / "latest.json"), "-input", str(artifacts))
                shutil.copy2(public, state / "public.key")
                run(updater, "-configure", "-config", str(state / "config.json"), "-dir", str(state), "-public", str(state / "public.key"),
                    "-api-url", origin, "-token-file", str(state / "token"), "-discover", str(prefix / "usr/local/bin/discover"), "-signed")
                wrapper = str(prefix / "usr/local/bin/homelab-discover")
                run(wrapper)
                assert json.loads((state / "state.json").read_text())["current"] == 1
                # Publish and accept sequence 2, then recover to 1 without lowering high.
                run(release_script, "release", "local-test")
                run(wrapper)
                status = json.loads((state / "state.json").read_text())
                assert status["current"] == status["high"] == 2 and not status["pending"]
                run(updater, "-dir", str(state), "-recover")
                status = json.loads((state / "state.json").read_text())
                assert status["current"] == 1 and status["high"] == 2
                # Installer rerun must retain the replay high-water mark.
                run(*args, "--signed", "--public-key", str(public))
                assert json.loads((state / "state.json").read_text())["high"] == 2
                run(installer, "--prefix", str(prefix), "--uninstall")
                assert not (state / "token").exists() and not (state / "config.json").exists()
                assert (state / "state.json").exists()
                assert not (prefix / "etc/systemd/system/homelab-discover.timer").exists()
                # A completely new host can start directly in signed mode, too.
                signed_prefix = tmp / "signed-root"
                run(installer, "--prefix", str(signed_prefix), "--api-url", origin,
                    "--token-file", str(token_file), "--binary-dir", str(artifacts),
                    "--signed", "--public-key", str(public),
                    "--manifest", str(release_tree / "latest.json"), "--artifacts", str(artifacts))
                assert json.loads((signed_prefix / "var/lib/homelab-agent/state.json").read_text())["current"] == 2
                run(installer, "--prefix", str(signed_prefix), "--uninstall")
            finally:
                proc.terminate()
                proc.wait(timeout=10)
    print("isolated installer + AGENT_TOKEN post/admin refusal + signed upgrade/rollback/uninstall: PASS")


if __name__ == "__main__":
    main()
