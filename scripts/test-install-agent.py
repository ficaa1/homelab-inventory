#!/usr/bin/env python3
"""Disposable installer/poster/update integration check; no root or systemd."""
import json
import fcntl
import os
from pathlib import Path
import platform
import re
import secrets
import signal
import shutil
import socket
import subprocess
import tempfile
import time
import hashlib
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
        revision = int(re.search(r"const AgentRevision = (\d+)", (ROOT / "internal/inventory/inventory.go").read_text()).group(1))
        raw = {"schema_version": "5", "collected_at": "__TIME__",
               "host": {"hostname": socket.gethostname()}, "tool": {"version": "local-test", "revision": revision},
               "docker": None, "diagnostics": [{"severity": "warning", "message": "synthetic partial inventory"}]}
        payload = json.dumps(raw)
        collector_script = ("#!/bin/sh\nif [ \"${1:-}\" = -list ]; then exit 0; fi\nprintf '%s' '" + payload
                            + "' | sed \"s/__TIME__/$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)/\"\n")
        collector_script = collector_script.replace("fi\nprintf", 'fi\n[ ! -e "' + str(tmp/'collect-fail') + '" ] || exit 1\nprintf')
        for target in ("amd64", "arm64"):
            binary = artifacts / f"discover-linux-{target}"
            binary.write_text(collector_script)
            binary.chmod(0o755)
        for target in ("amd64", "arm64"):
            for name in ("agentupdate", "agentboot"):
                shutil.copy2(ROOT / "bin" / f"{name}-linux-{target}", artifacts)

        # Test-only launcher contains synthetic mount and process-cutover seams;
        # release builds have neither environment hook.
        run_build = subprocess.run(["nice", "-n", "19", "ionice", "-c3", "go", "build",
            "-tags=agentinstalltest", "-o", str(artifacts / f"agentboot-linux-{arch}"), "./cmd/agentboot"],
            cwd=ROOT, capture_output=True, timeout=120)
        if run_build.returncode:
            raise RuntimeError("test launcher build failed")
        packaged = tmp / "packaged"
        for target in ("amd64", "arm64"):
            (packaged / f"linux-{target}").mkdir(parents=True)
            for name in ("discover", "agentupdate", "agentboot"):
                shutil.copy2(artifacts / f"{name}-linux-{target}", packaged / f"linux-{target}" / name)
        # Fixtures are copied below after synthetic discover/updater creation.
        sums = []
        for path in sorted(packaged.glob("linux-*/*")):
            sums.append(hashlib.sha256(path.read_bytes()).hexdigest() + "  " + str(path.relative_to(packaged)))
        (packaged / "SHA256SUMS").write_text("\n".join(sums) + "\n")
        env["AGENT_BIN_DIR"] = str(packaged)

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
                assert first["agent_update"]["mode"] == "unsigned" and first["agent_update"]["updater"]
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
                # Convert a legacy signed installation in-place, preserving its
                # same-key high-water mark and hostname/history.
                legacy_args = [installer, "--prefix", str(prefix), "--api-url", origin,
                    "--token-file", str(token_file), "--binary-dir", str(artifacts),
                    "--updates", "slots", "--public-key", str(public),
                    "--manifest", str(release_tree / "2/runtime.json"), "--artifacts", str(artifacts)]
                refused = subprocess.run(legacy_args, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert refused.returncode != 0  # high=2,current=1: sequence 2 is rejected
                run(release_script, "release", "local-test")
                legacy_args[legacy_args.index(str(release_tree / "2/runtime.json"))] = str(release_tree / "3/runtime.json")
                run(*legacy_args)
                assert json.loads((state / "control.0").read_text())["payload"]["floor"] == 3
                assert json.loads((state / "state.json").read_text())["high"] == 2
                assert latest()["host"]["hostname"] == socket.gethostname()
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
                assert json.loads((signed_prefix / "var/lib/homelab-agent/state.json").read_text())["current"] == 3
                run(installer, "--prefix", str(signed_prefix), "--uninstall")
                run(release_script, "hold")
                # Fresh slots: real launcher and updater, signed baseline twice.
                slots_prefix = tmp / "slots-root"
                slots_args = [installer, "--prefix", str(slots_prefix), "--api-url", origin,
                    "--token-file", str(token_file), "--binary-dir", str(artifacts),
                    "--updates", "slots", "--public-key", str(public),
                    "--manifest", str(release_tree / "2/runtime.json"), "--artifacts", str(artifacts)]
                run(*slots_args)
                slots = slots_prefix / "var/lib/homelab-agent"
                assert latest()['agent_update']['launcher']
                def control():
                    return json.loads((slots / "control.0").read_text())["payload"]
                assert control()["floor"] == 2 and control()["state"] == "committed"
                assert len(list((slots / "slots/a").glob("*/discover"))) == 1
                assert len(list((slots / "slots/b").glob("*/agentupdate"))) == 1
                assert "agentboot -config" in (slots_prefix / "etc/systemd/system/homelab-discover.service").read_text()
                # Omitting the slots flag must not silently replace its schedule
                # with an unsigned poster that bypasses the reserved floor.
                refused = subprocess.run([installer, "--prefix", str(slots_prefix), "--api-url", origin,
                    "--token-file", str(token_file), "--binary-dir", str(artifacts)],
                    cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert refused.returncode != 0 and control()["floor"] == 2
                run(*slots_args)  # installer upgrades launcher without resetting floor
                run(release_script, "release", "local-test")
                run(str(slots_prefix / "usr/local/bin/homelab-discover"))
                assert control()["floor"] == 4 and control()["active"]["sequence"] == 4
                assert control()["fallback"]["sequence"] == 2 and control()["receipt"]["identity"]["sequence"] == 4
                run(installer, "--prefix", str(slots_prefix), "--uninstall")
                assert (slots / "control.0").exists() and not (slots / "token").exists()
                def admin(method, path, body=None):
                    data = json.dumps(body).encode() if body is not None else None
                    req = urllib.request.Request(origin + path, method=method, data=data,
                        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
                    with urllib.request.urlopen(req, timeout=5) as response:
                        return json.load(response)

                def code():
                    return admin("POST", "/setup/enroll", {"hostname": socket.gethostname()})["code"]

                # Signed served enrollment authenticates key, baseline, artifact
                # and installed launcher downloads; only bootstrap tools public.
                enrolled = tmp / "enrolled"
                fresh_code = code()
                served = [installer, "--prefix", str(enrolled), "--api-url", origin,
                    "--download", "--updates", "slots", "--enroll", fresh_code,
                    "--hostname", socket.gethostname()]
                run(*served)
                es = enrolled / "var/lib/homelab-agent"
                assert json.loads((es / ".migration/intent.json").read_text())["phase"] == "committed"
                assert (es / "public.key").read_bytes() == public.read_bytes()
                assert (es / "token").stat().st_mode & 0o777 == 0o600
                nonce = json.loads((es / ".migration/enroll.token.enrollment").read_text())["nonce"]
                # Same code+nonce can recover a lost redemption response; another
                # installation nonce must not reuse an already consumed code.
                redeem = {"code": fresh_code, "hostname": socket.gethostname(), "nonce": nonce}
                retry = urllib.request.Request(origin + "/agent/enroll", data=json.dumps(redeem).encode(),
                    headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(retry, timeout=5) as response:
                    assert json.load(response)["agent_token"] == agent_token
                redeem["nonce"] = secrets.token_hex(16)
                retry.data = json.dumps(redeem).encode()
                try:
                    urllib.request.urlopen(retry, timeout=5)
                    raise AssertionError("consumed enrollment code accepted with a new nonce")
                except urllib.error.HTTPError as error:
                    assert error.code == 403

                # Fake systemd permits real cutover failure and signal tests
                # without touching this machine's services.
                fakebin = tmp / "fakebin"
                fakebin.mkdir()
                fake_systemd = fakebin / "systemctl"
                fake_systemd.write_text("""#!/usr/bin/env python3
import json, os, pathlib, shlex, signal, subprocess, sys, time
root = pathlib.Path(os.environ['TEST_SCHEDULE_ROOT'])
log = root / 'systemd.log'
with log.open('a') as f: f.write(' '.join(sys.argv[1:]) + '\\n')
mode = root / 'interrupt'
if mode.exists() and sys.argv[1:] == ['enable', '--now', 'homelab-discover.timer']:
 sig=int(mode.read_text()); mode.unlink(); os.kill(os.getppid(), sig)
lock_request = root/'report-lock'
if lock_request.exists() and sys.argv[1:]==['enable', '--now', 'homelab-discover.timer']:
 lock_path=lock_request.read_text(); lock_request.unlink()
 ready=root/'lock-ready'
 child="import fcntl,pathlib,sys,time; f=open(sys.argv[1],'w'); pathlib.Path(sys.argv[2]).touch(); fcntl.flock(f,fcntl.LOCK_EX); time.sleep(2)"
 subprocess.Popen([sys.executable,'-c',child,lock_path,str(ready)], stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 while not ready.exists(): time.sleep(.01)
 ready.unlink()
active=root/'timer-active'
if sys.argv[1:2]==['stop'] and sys.argv[-1]=='homelab-discover.timer': active.unlink(missing_ok=True)
if sys.argv[1:2] in (['enable'], ['start']) and sys.argv[-1]=='homelab-discover.timer': active.touch()
if sys.argv[1:]==['tick']:
 assert active.exists(), 'timer was left stopped'
 unit=pathlib.Path(os.environ['TEST_TIMER_UNIT']).read_text()
 command=next(line.removeprefix('ExecStart=') for line in unit.splitlines() if line.startswith('ExecStart='))
 sys.exit(subprocess.run(shlex.split(command)).returncode)
fail = root / 'fail'
if fail.exists() and sys.argv[1:3] == ['enable', '--now']:
 fail.unlink(); sys.exit(1)
if sys.argv[1:] == ['start', 'homelab-discover.service'] and os.environ.get('TEST_SCHEDULE_WRAPPER'):
 sys.exit(subprocess.run([os.environ['TEST_SCHEDULE_WRAPPER']], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode)
sys.exit(0)
""")
                fake_systemd.chmod(0o755)
                env['PATH'] = str(fakebin) + os.pathsep + env['PATH']
                env['AGENTINSTALL_TEST_SYSTEMD'] = '1'
                env['TEST_SCHEDULE_ROOT'] = str(tmp)
                migrated = tmp / "migration"
                plain = [installer, "--prefix", str(migrated), "--api-url", origin,
                    "--token-file", str(token_file), "--binary-dir", str(artifacts)]
                run(*plain)
                ms = migrated / "var/lib/homelab-agent"
                old_config = (ms / "config.json").read_bytes()
                old_service = (migrated / "etc/systemd/system/homelab-discover.service").read_bytes()
                conversion = [installer, "--prefix", str(migrated), "--api-url", origin,
                    "--download", "--updates", "slots", "--enroll", code()]
                env["TEST_SCHEDULE_WRAPPER"] = str(migrated / "usr/local/bin/homelab-discover")
                (tmp / "fail").touch()
                result = subprocess.run(conversion, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert (ms / "config.json").read_bytes() == old_config
                assert (migrated / "etc/systemd/system/homelab-discover.service").read_bytes() == old_service
                assert json.loads((ms / ".migration/intent.json").read_text())["phase"] == "prepared"
                # Already redeemed token resumes even if enrollment would fail.
                # A legacy report holding the shared lock must finish without
                # making the paste fail. Keep the lock until the wait is visible.
                with (ms / 'lock').open('w') as held:
                    fcntl.flock(held, fcntl.LOCK_EX)
                    paste = subprocess.Popen(conversion, cwd=ROOT, env=env,
                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                    line = paste.stderr.readline()
                    assert b'waiting for the running agent to finish' in line, line
                    fcntl.flock(held, fcntl.LOCK_UN)
                    _, error = paste.communicate(timeout=60)
                    assert paste.returncode == 0, error[:500]
                assert json.loads((ms / "control.0").read_text())["payload"]["floor"] == 4
                # Existing-slots rollback must restart its old launcher without
                # recursively resuming the installer under its own lock.
                reinstall = list(conversion)
                reinstall[reinstall.index('--enroll') + 1] = code()
                (tmp / 'fail').touch()
                result = subprocess.run(reinstall, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert json.loads((ms / '.migration/intent.json').read_text())['phase'] == 'prepared'
                (tmp/'report-lock').write_text(str(ms/'lock'))
                result = run(*reinstall)
                assert b'waiting for the running agent to finish' in result.stderr
                # A fresh code recovers an obsolete installed credential without
                # changing the installed token during preparation.
                (ms / 'token').write_text(secrets.token_hex(32)+'\n')
                reinstall[reinstall.index('--enroll') + 1] = code()
                run(*reinstall)
                assert (ms / 'token').read_text().strip() == agent_token
                env.pop('TEST_SCHEDULE_WRAPPER')
                # Every signal occurs after recovery entry points are published,
                # before the first report. Only a later timer tick may recover.
                for death in (signal.SIGKILL, signal.SIGINT, signal.SIGHUP, signal.SIGTERM):
                    interrupted = tmp / f"interrupted-{int(death)}"
                    killed_plain = list(plain)
                    killed_plain[killed_plain.index(str(migrated))] = str(interrupted)
                    run(*killed_plain)
                    interruption = [installer, "--prefix", str(interrupted), "--api-url", origin,
                        "--download", "--updates", "slots", "--token-file", str(token_file)]
                    (tmp / "interrupt").write_text(str(int(death)))
                    result = subprocess.run(interruption, cwd=ROOT, env=env, capture_output=True, timeout=60)
                    assert result.returncode != 0
                    ins = interrupted / "var/lib/homelab-agent"
                    phase = json.loads((ins / ".migration/intent.json").read_text())["phase"]
                    assert phase == ("switching" if death == signal.SIGKILL else "prepared")
                    env['TEST_TIMER_UNIT'] = str(interrupted/'etc/systemd/system/homelab-discover.service')
                    run(str(fake_systemd), 'tick')
                    assert (tmp/'timer-active').exists()
                    if death == signal.SIGKILL:
                        assert json.loads((ins / ".migration/intent.json").read_text())["phase"] == "committed"
                    run(str(fake_systemd), 'tick')  # working scheduled report after recovery
                # A fresh host has nothing to restore. Its retained schedule
                # must finish the migration once the collector recovers.
                fresh = tmp/'fresh-retry'
                fresh_install = [installer, '--prefix', str(fresh), '--api-url', origin,
                    '--download', '--updates', 'slots', '--token-file', str(token_file)]
                (tmp/'collect-fail').touch()
                result = subprocess.run(fresh_install, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert b'first signed report failed; the new schedule will retry' in result.stderr
                fresh_state = fresh/'var/lib/homelab-agent'
                assert json.loads((fresh_state/'.migration/intent.json').read_text())['phase']=='switching'
                (tmp/'collect-fail').unlink()
                env['TEST_TIMER_UNIT'] = str(fresh/'etc/systemd/system/homelab-discover.service')
                run(str(fake_systemd), 'tick')
                assert json.loads((fresh_state/'.migration/intent.json').read_text())['phase']=='committed'
                run(str(fake_systemd), 'tick')
                env.pop('AGENTINSTALL_TEST_SYSTEMD')

                # TrueNAS config-DB fixture: real signed launcher/report, fake
                # middleware only. All files stay below disposable temp state.
                fake_id = fakebin / 'id'
                fake_id.write_text('#!/bin/sh\nprintf "0\\n"\n')
                fake_id.chmod(0o755)
                dataset = tmp / 'dataset'
                dataset.mkdir()
                mounts = tmp / 'mountinfo'
                mounts.write_text(f"1 0 0:1 / / rw - rootfs rootfs rw\n2 1 0:2 / {dataset} rw - zfs pool/agents rw\n")
                env['AGENTINSTALL_TEST_MOUNTINFO'] = str(mounts)
                zfsfile = tmp / 'zfs-list'
                zfsfile.write_text(f'tank/agents\t{dataset}\tyes\n')
                env['TEST_ZFS_LIST'] = str(zfsfile)
                zfs = fakebin / 'zfs'
                zfs.write_text("#!/usr/bin/env python3\nimport os, pathlib, sys\nassert sys.argv[1:]==['list', '-H', '-o', 'name,mountpoint,mounted']\nsys.stdout.write(pathlib.Path(os.environ['TEST_ZFS_LIST']).read_text())\n")
                zfs.chmod(0o755)
                jobsfile = tmp / 'cron.json'
                predecessor = {'id': 7, 'user': 'root', 'command': '/mnt/example/previous/agent',
                    'description': 'previous-agent', 'enabled': True, 'stdout': False, 'stderr': False,
                    'schedule': {'minute': '*/15', 'hour': '*', 'dom': '*', 'month': '*', 'dow': '*'}}
                jobsfile.write_text(json.dumps([predecessor]))
                env['TEST_CRON_DB'] = str(jobsfile)
                midclt = fakebin / 'midclt'
                midclt.write_text("""#!/usr/bin/env python3
import json, os, pathlib, sys
p=pathlib.Path(os.environ['TEST_CRON_DB']); jobs=json.loads(p.read_text())
method=sys.argv[2].split('.')[-1]; args=sys.argv[3:]
if pathlib.Path(str(p)+'.offline').exists(): sys.exit(1)
if method=='query': result=jobs
elif method=='create':
 result=json.loads(args[0]); result['id']=max([j['id'] for j in jobs]+[0])+1; jobs.append(result)
elif method=='update':
 result=next(j for j in jobs if j['id']==int(args[0])); result.update(json.loads(args[1]))
else:
 jobs=[j for j in jobs if j['id']!=int(args[0])]; result=True
p.write_text(json.dumps(jobs))
if method=='update' and pathlib.Path(str(p)+'.fail').exists() and int(args[0])==7 and not result['enabled']:
 pathlib.Path(str(p)+'.fail').unlink()
 if pathlib.Path(str(p)+'.break-rollback').exists(): pathlib.Path(str(p)+'.offline').touch()
 sys.exit(1)
print(json.dumps(result))
""")
                midclt.chmod(0o755)
                cronstate = dataset / 'agent'
                cronargs = [installer, '--api-url', origin, '--updates', 'slots',
                    '--scheduler', 'truenas-cron', '--state-dir', str(cronstate),
                    '--replace-schedule', 'previous-agent', '--binary-dir', str(artifacts),
                    '--token-file', str(token_file), '--public-key', str(public),
                    '--manifest', str(release_tree / '4/runtime.json'), '--artifacts', str(artifacts)]
                # Exercise served bootstrap too, with only its /mnt prefix
                # redirected to the disposable synthetic mount fixture.
                cron_installer = tmp/'install-cron.sh'
                cron_installer.write_text(installer_copy.read_text().replace(
                    'case $state in /mnt/*)', f'case $state in {dataset}/*)'))
                cron_installer.chmod(0o755)
                findmnt = fakebin/'findmnt'
                findmnt.write_text(f'#!/bin/sh\nprintf "%s zfs rw\\n" "{dataset}"\n')
                findmnt.chmod(0o755)
                served_cronargs = list(cronargs)
                served_cronargs[0] = str(cron_installer)
                served_cronargs.append('--download')
                # Mount roots and non-dedicated share directories retain mode/data.
                shared = dataset/'share'
                shared.mkdir(mode=0o755)
                (shared/'unrelated').write_text('keep')
                empty = dataset/'existing-empty'
                empty.mkdir(mode=0o755)
                malformed = dataset/'malformed'
                malformed.mkdir(mode=0o755)
                (malformed/'.homelab-inventory').write_text('homelab-inventory\n\n')
                child = dataset/'locked'
                child.mkdir(mode=0o700)
                zfsfile.write_text(f'boot-pool/ROOT/previous\t/\tno\ntank/agents\t{dataset}\tyes\ntank/locked\t{child}\tno\n')
                for refused in (dataset, shared, empty, malformed, child, child/'agent'):
                    mode = refused.stat().st_mode if refused.exists() else None
                    for command in (cronargs, served_cronargs):
                        bad = list(command); bad[bad.index(str(cronstate))] = str(refused)
                        result = subprocess.run(bad, cwd=ROOT, env=env, capture_output=True, timeout=60)
                        assert result.returncode != 0
                        assert (refused.stat().st_mode if refused.exists() else None) == mode
                        assert not (refused/'.migration').exists()
                        if refused != malformed:
                            assert not (refused/'.homelab-inventory').exists()
                # An inactive TrueNAS boot environment is an unmounted dataset at /.
                zfsfile.write_text(f'boot-pool/ROOT/previous\t/\tno\ntank/agents\t{dataset}\tyes\n')
                # Failure after the predecessor was disabled must re-enable it
                # and remove the newly created replacement job.
                Path(str(jobsfile)+'.fail').touch()
                result = subprocess.run(cronargs, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert json.loads(jobsfile.read_text()) == [predecessor]
                # Failed middleware rollback must retain the executable/config,
                # including across boot-time Resume failures, until a later tick.
                Path(str(jobsfile)+'.fail').touch()
                Path(str(jobsfile)+'.break-rollback').touch()
                result = subprocess.run(cronargs, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert (cronstate/'agentboot').exists() and (cronstate/'config.json').exists()
                assert (cronstate/'token').exists()
                resume = [str(cronstate/'agentboot'), '-config', str(cronstate/'config.json'), '-log']
                result = subprocess.run(resume, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert (cronstate/'agentboot').exists()
                assert json.loads((cronstate/'.migration/intent.json').read_text())['phase']=='switching'
                Path(str(jobsfile)+'.offline').unlink()
                Path(str(jobsfile)+'.break-rollback').unlink()
                run(*resume)
                assert (cronstate/'launcher.log').stat().st_mode & 0o777 == 0o600
                jobs = json.loads(jobsfile.read_text())
                assert len(jobs) == 2 and not next(j for j in jobs if j['id']==7)['enabled']
                own = next(j for j in jobs if j['description']=='homelab-inventory-agent')
                assert own['user']=='root' and own['enabled'] and own['schedule']['minute']=='*/15'
                assert own['stdout'] and own['stderr'] and own['command'].endswith(' -log')
                assert str(cronstate / 'agentboot') in own['command']
                run(*served_cronargs)  # stable ID, exactly one owned job
                assert len(json.loads(jobsfile.read_text())) == 2
                assert (cronstate / 'token').stat().st_mode & 0o777 == 0o600
                assert not (cronstate / 'usr').exists() and not (cronstate / 'etc').exists()
                saved_jobs = json.loads(jobsfile.read_text())
                for duplicate in ('identity', 'command', 'predecessor'):
                    extra = dict(own if duplicate != 'predecessor' else predecessor)
                    extra['id'] = 99
                    if duplicate == 'command': extra['description'] = 'renamed-agent'
                    jobsfile.write_text(json.dumps(saved_jobs + [extra]))
                    result = subprocess.run(cronargs, cwd=ROOT, env=env, capture_output=True, timeout=60)
                    assert result.returncode != 0
                    assert json.loads(jobsfile.read_text()) == saved_jobs + [extra]
                jobsfile.write_text(json.dumps(saved_jobs))
                # A prepared conversion must re-read the legacy high-water mark:
                # the restored legacy timer may have rejected a newer release.
                floor_prefix = tmp / 'floor-advance'
                floor_plain = [installer, '--prefix', str(floor_prefix), '--api-url', origin,
                    '--token-file', str(token_file), '--binary-dir', str(artifacts), '--signed',
                    '--public-key', str(public), '--manifest', str(release_tree / 'latest.json'),
                    '--artifacts', str(artifacts)]
                run(*floor_plain)
                env['AGENTINSTALL_TEST_SYSTEMD'] = '1'
                env['TEST_SCHEDULE_WRAPPER'] = str(floor_prefix/'usr/local/bin/homelab-discover')
                floor_conversion = [installer, '--prefix', str(floor_prefix), '--api-url', origin,
                    '--download', '--updates', 'slots', '--enroll', code()]
                (tmp/'fail').touch()
                result = subprocess.run(floor_conversion, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                fs = floor_prefix/'var/lib/homelab-agent'
                assert (fs/'.migration/intent.json').exists(), result.stderr.decode()
                legacy = json.loads((fs/'state.json').read_text())
                legacy['high'] = 5
                legacy['update']['rejected'] = 5
                (fs/'state.json').write_text(json.dumps(legacy))
                result = subprocess.run(floor_conversion, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0
                assert json.loads((fs/'state.json').read_text())['high'] == 5
                assert json.loads((fs/'.migration/intent.json').read_text())['phase'] == 'prepared'
                old_mirror = (fs/'control.1').read_bytes()
                old_generation = json.loads(old_mirror)['payload']['generation']
                run(release_script, 'release', 'local-test')
                run(release_script, 'release', 'local-test')
                floor_conversion[floor_conversion.index('--enroll')+1] = code()
                run(*floor_conversion)
                assert json.loads((fs/'control.0').read_text())['payload']['floor'] == 6
                assert json.loads((fs/'state.json').read_text())['high'] == 5
                assert json.loads((fs/'control.0').read_text())['payload']['generation'] > old_generation
                # Emulate interruption between mirror writes in the repair:
                # the new floor must remain readable beside the old generation.
                (fs/'control.1').write_bytes(old_mirror)
                status = run(str(fs/'agentboot'), '-dir', str(fs), '-status')
                assert json.loads(status.stdout)['floor'] == 6
                run(*floor_conversion)
                assert json.loads((fs/'control.1').read_text())['payload']['floor'] == 6
                env.pop('TEST_SCHEDULE_WRAPPER')
                env.pop('AGENTINSTALL_TEST_SYSTEMD')
                # Unmounted/noexec datasets refuse before creating agent state.
                mounts.write_text('1 0 0:1 / / rw - rootfs rootfs rw\n')
                badargs = list(cronargs); badargs[badargs.index(str(cronstate))] = str(dataset/'empty')
                result = subprocess.run(badargs, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0 and not (dataset/'empty').exists()
                mounts.write_text(f"1 0 0:1 / / rw - rootfs rootfs rw\n2 1 0:2 / {dataset} rw,noexec - zfs pool/agents rw\n")
                result = subprocess.run(badargs, cwd=ROOT, env=env, capture_output=True, timeout=60)
                assert result.returncode != 0 and not (dataset/'empty').exists()
            finally:
                proc.terminate()
                proc.wait(timeout=10)
    print("isolated installer: signed enrollment, legacy/unsigned conversion, durable resume/rollback, cron adoption, credential boundaries: PASS")


if __name__ == "__main__":
    main()
