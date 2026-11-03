package agentboot

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentreceipt"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

type runtimeLab struct {
	t                            *testing.T
	boot, config                 string
	r                            Runner
	key                          ed25519.PrivateKey
	discover, updater            []byte
	manifest                     []byte
	files                        map[string][]byte
	mu                           sync.Mutex
	offer, partial, reject, lost bool
	statuses                     []agentproto.Update
	posts                        []agentproto.Receipt
	receiptDir                   string
}

func compile(t *testing.T, root, out, pkg string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", append(append([]string{"build", "-trimpath"}, args...), "-o", out, pkg)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build fixture: %v %s", e, b)
	}
}
func (l *runtimeLab) publish(seq uint64, mode string) {
	l.t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	files := map[string][]byte{"discover": append(append([]byte(nil), l.discover...), []byte("\nFIXTURE:"+mode)...), "agentupdate": l.updater}
	if strings.HasPrefix(mode, "worker:") {
		files["discover"] = append(append([]byte(nil), l.discover...), []byte("\nFIXTURE:good")...)
		files["agentupdate"] = append(append([]byte(nil), l.discover...), []byte("\nFIXTURE:"+strings.TrimPrefix(mode, "worker:"))...)
	}
	m := agentrelease.Manifest{Product: agentproto.Product, LauncherProtocol: 1, RuntimeCompatibility: 1, Platforms: []string{platform()}, Sequence: seq, Revision: 1, Version: "fixture", Created: time.Now().UTC(), Artifacts: map[string]agentrelease.Artifact{}}
	for role, b := range files {
		name := role + "-" + platform()
		m.Artifacts[platform()+"/"+role] = agentrelease.Artifact{Role: role, Name: name, Size: int64(len(b)), SHA256: agentrelease.Digest(b)}
	}
	raw, _ := json.Marshal(m)
	l.manifest, _ = json.Marshal(agentrelease.Signed{Manifest: raw, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(l.key, raw))})
	l.files = files
	l.offer = true
}
func (l *runtimeLab) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture-token" {
		w.WriteHeader(403)
		return
	}
	switch {
	case r.URL.Path == "/agent/manifest":
		if !l.offer {
			w.WriteHeader(204)
			return
		}
		w.Write(l.manifest)
	case strings.HasPrefix(r.URL.Path, "/agent/artifacts/"):
		role := strings.TrimSuffix(filepath.Base(r.URL.Path), "-"+platform())
		b := l.files[role]
		if l.partial && len(b) > 0 {
			b = b[:len(b)/2]
		}
		w.Write(b)
	case r.URL.Path == "/agent/status":
		var d struct {
			Update agentproto.Update `json:"update"`
		}
		if json.NewDecoder(r.Body).Decode(&d) == nil {
			l.statuses = append(l.statuses, d.Update)
		}
		w.WriteHeader(204)
	case r.URL.Path == "/agent/report":
		b, e := io.ReadAll(r.Body)
		var data struct {
			Host    struct{ Hostname string }
			Runtime agentproto.Runtime `json:"agent_runtime"`
		}
		if e != nil || json.Unmarshal(b, &data) != nil {
			w.WriteHeader(400)
			return
		}
		if _, e = agentproto.ValidateReport(b, data.Host.Hostname, data.Runtime.Nonce, data.Runtime.Identity); e != nil {
			w.WriteHeader(400)
			return
		}
		if l.reject && data.Runtime.Sequence == 2 {
			w.WriteHeader(503)
			return
		}
		rec, e := agentreceipt.Accept(l.receiptDir, data.Host.Hostname, data.Runtime, b, func(rec agentproto.Receipt) error { l.posts = append(l.posts, rec); return nil })
		if e != nil {
			w.WriteHeader(500)
			return
		}
		// Preserve byte-for-byte evidence, including fields the launcher never knew.
		if !strings.Contains(string(b), `"future_field":{"n":7}`) || !strings.Contains(string(b), `"docker":null`) {
			l.t.Error("report fields lost")
		}
		if l.lost && rec.Identity.Sequence == 2 {
			l.lost = false
			h := w.(http.Hijacker)
			c, _, _ := h.Hijack()
			c.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(rec)
	default:
		w.WriteHeader(404)
	}
}
func (l *runtimeLab) run(fault, mode string, skip int) error {
	l.t.Helper()
	cmd := exec.Command(l.boot, "-config", l.config, "-timeout", "400ms")
	cmd.Env = append(os.Environ(), "AGENTBOOT_FAULT="+fault, "AGENTBOOT_FAULT_MODE="+mode, fmt.Sprintf("AGENTBOOT_FAULT_SKIP=%d", skip))
	b, e := cmd.CombinedOutput()
	if e != nil && len(b) > 0 && !strings.Contains(string(b), "operation failed") {
		l.t.Fatalf("unexpected binary output: %s", b)
	}
	return e
}
func (l *runtimeLab) load() Control {
	l.t.Helper()
	c, e := l.r.Load()
	if e != nil {
		l.t.Fatal(e)
	}
	return c
}
func (l *runtimeLab) assertRunnable(c Control) {
	l.t.Helper()
	for _, s := range []Slot{c.Active, c.Fallback} {
		if _, e := l.r.checkSlot(s); e != nil {
			l.t.Fatalf("lost runnable pair: %v", e)
		}
	}
}

// One table-driven harness exercises real agentboot and agentupdate binaries,
// signed executable fixtures, real pipes/process groups and durable receipts.
func TestRuntimeFaults(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process-group and subreaper harness")
	}
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	bin := t.TempDir()
	boot := filepath.Join(bin, "agentboot")
	updater := filepath.Join(bin, "agentupdate")
	discover := filepath.Join(bin, "collector")
	compile(t, root, boot, "./cmd/agentboot", "-tags=agentbootfault")
	compile(t, root, updater, "./cmd/agentupdate")
	compile(t, root, discover, "./internal/agentboot/testdata/collector.go")
	ub, _ := os.ReadFile(updater)
	db, _ := os.ReadFile(discover)
	type scenario struct {
		name, kind, point, mode string
		skip                    int
	}
	cases := []scenario{}
	for _, state := range []string{"prepared", "pending", "report-pending", "committed", "quarantined"} {
		for _, mirror := range []string{"control.0", "control.1"} {
			for _, boundary := range []string{"fsync", "rename", "dirsync"} {
				count := 1
				if state == "report-pending" {
					count = 3
				}
				for i := 0; i < count; i++ {
					skip := i
					if state == "committed" {
						skip = 1
					}
					point := state + "-" + mirror + "-" + boundary
					cases = append(cases, scenario{fmt.Sprintf("kill/%s/%d", point, skip), "kill", point, "", skip})
				}
			}
		}
	}
	for _, point := range []string{"discover-fsync", "agentupdate-fsync", "envelope-fsync", "envelope-rename", "envelope-dirsync", "slot-before", "slot-rename", "slot-dirsync", "report-fsync", "report-rename", "report-dirsync", "receipt-before", "receipt-accepted"} {
		cases = append(cases, scenario{"kill/" + point, "kill", point, "", 0})
	}
	for _, mode := range []string{"silent", "malformed", "oversized", "exit", "hung", "wrong-build", "duplicate", "errors"} {
		cases = append(cases, scenario{"trial/" + mode, "trial", "", mode, 0})
	}
	for _, mode := range []string{"silent", "malformed", "oversized", "exit", "hung"} {
		cases = append(cases, scenario{"worker/" + mode, "worker", "", mode, 0})
	}
	for _, kind := range []string{"partial", "signature", "hash", "platform", "roles", "traversal", "space", "inode-space", "journal-space", "mirror-space", "fsync", "lock", "lost", "rejected", "current-delivery", "backward", "baseline-errors", "later", "dead-updater", "one-record", "both-records", "prune", "held"} {
		cases = append(cases, scenario{kind, kind, "", "", 0})
	}
	for _, kind := range []string{"trial-host-errors", "trial-host-timeout", "trial-fallback-exit", "trial-host-timeout-bound", "committed-host-errors", "committed-candidate-errors", "committed-host-timeout", "committed-candidate-timeout", "committed-host-exit", "committed-host-invalid", "committed-host-identity", "prepare-host-exit"} {
		cases = append(cases, scenario{kind, kind, "", "", 0})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() {
				_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
					if e == nil && d.IsDir() {
						_ = os.Chmod(p, 0700)
					}
					return nil
				})
			})
			public, key, _ := ed25519.GenerateKey(rand.Reader)
			pubPath := filepath.Join(dir, "public.key")
			if e = os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(public)), 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(dir, "token"), []byte("fixture-token"), 0600); e != nil {
				t.Fatal(e)
			}
			l := &runtimeLab{t: t, boot: boot, r: Runner{Config: Config{Dir: dir, Public: pubPath}}, key: key, discover: db, updater: ub, receiptDir: t.TempDir()}
			api := httptest.NewServer(http.HandlerFunc(l.serve))
			defer api.Close()
			l.r.URL = api.URL
			l.r.TokenFile = filepath.Join(dir, "token")
			l.config = filepath.Join(dir, "config.json")
			b, _ := json.Marshal(l.r.Config)
			os.WriteFile(l.config, b, 0600)
			baselineMode := "good"
			if tc.kind == "baseline-errors" {
				baselineMode = "errors"
			}
			if tc.kind == "prepare-host-exit" {
				baselineMode = "exit"
			}
			l.publish(1, baselineMode)
			baseline := t.TempDir()
			for role, data := range l.files {
				os.WriteFile(filepath.Join(baseline, role+"-"+platform()), data, 0755)
			}
			manifest := filepath.Join(baseline, "runtime.json")
			os.WriteFile(manifest, l.manifest, 0600)
			init := exec.Command(boot, "-dir", dir, "-public", pubPath, "-initialize", manifest, "-input", baseline)
			if b, e := init.CombinedOutput(); e != nil {
				t.Fatalf("initialize: %v %s", e, b)
			}
			c := l.load()
			l.assertRunnable(c)
			candidateMode := "good"
			if tc.kind == "trial" {
				candidateMode = tc.mode
				if tc.mode == "errors" {
					candidateMode = "trigger-extra-errors"
				}
			}
			if tc.kind == "worker" {
				candidateMode = "worker:" + tc.mode
			}
			if tc.kind == "baseline-errors" {
				candidateMode = "errors"
			}
			if tc.kind == "later" {
				candidateMode = "later"
			}
			if tc.kind == "backward" {
				candidateMode = "backward"
			}
			if strings.HasPrefix(tc.point, "quarantined") {
				candidateMode = "exit"
			}
			switch tc.kind {
			case "trial-host-errors":
				candidateMode = "trigger-errors"
			case "trial-fallback-exit":
				candidateMode = "trigger-fallback-exit"
			case "trial-host-timeout", "trial-host-timeout-bound":
				candidateMode = "trigger-timeout"
			case "committed-candidate-errors":
				candidateMode = "later-errors"
			case "committed-candidate-timeout":
				candidateMode = "later-timeout"
			}
			l.publish(2, candidateMode)
			switch tc.kind {
			case "partial":
				l.partial = true
			case "signature":
				var v agentrelease.Signed
				json.Unmarshal(l.manifest, &v)
				v.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
				l.manifest, _ = json.Marshal(v)
			case "hash":
				l.files["discover"] = append([]byte(nil), l.files["discover"]...)
				l.files["discover"][0] ^= 1
			case "platform", "roles", "traversal":
				var v agentrelease.Signed
				json.Unmarshal(l.manifest, &v)
				var m agentrelease.Manifest
				json.Unmarshal(v.Manifest, &m)
				if tc.kind == "platform" {
					m.Platforms = []string{"linux-arm64"}
					if platform() == "linux-arm64" {
						m.Platforms = []string{"linux-amd64"}
					}
					arts := map[string]agentrelease.Artifact{}
					for _, role := range []string{"discover", "agentupdate"} {
						a := m.Artifacts[platform()+"/"+role]
						a.Name = role + "-" + m.Platforms[0]
						arts[m.Platforms[0]+"/"+role] = a
					}
					m.Artifacts = arts
				} else {
					a := m.Artifacts[platform()+"/discover"]
					if tc.kind == "roles" {
						a.Role = "launcher"
					} else {
						a.Name = "../escape"
					}
					m.Artifacts[platform()+"/discover"] = a
				}
				v.Manifest, _ = json.Marshal(m)
				v.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, v.Manifest))
				l.manifest, _ = json.Marshal(v)
			case "space":
				tc.point = "space"
				tc.mode = "enospc"
			case "inode-space":
				tc.point = "space-inodes"
				tc.mode = "enospc"
			case "journal-space":
				tc.point = "prepared-control.0-before"
				tc.mode = "enospc"
			case "mirror-space":
				tc.point = "prepared-control.1-before"
				tc.mode = "enospc"
			case "fsync":
				tc.point = "prepared-control.1-fsync"
				tc.mode = "eio"
			case "rejected":
				l.reject = true
			case "lost":
				l.lost = true
			case "held":
				l.offer = false
			}
			if tc.kind == "lock" {
				unlock, e := fsx.LockDir(dir)
				if e != nil {
					t.Fatal(e)
				}
				if l.run("", "", 0) == nil {
					t.Fatal("busy lock accepted")
				}
				// Status remains readable; the lock inode survives competing invocations.
				if _, e = l.r.Load(); e != nil {
					t.Fatal(e)
				}
				unlock()
				if len(l.statuses) == 0 || l.statuses[len(l.statuses)-1].Code != "lock_busy" {
					t.Fatal("missing lock status")
				}
				return
			}
			if tc.kind == "one-record" || tc.kind == "both-records" {
				os.WriteFile(filepath.Join(dir, "control.0"), []byte("corrupt"), 0600)
				if tc.kind == "both-records" {
					os.WriteFile(filepath.Join(dir, "control.1"), []byte("corrupt"), 0600)
					if l.run("", "", 0) == nil {
						t.Fatal("lost floor accepted")
					}
					if len(l.posts) != 0 || l.statuses[len(l.statuses)-1].Code != "state_corrupt" {
						t.Fatal("corrupt floor executed")
					}
					return
				}
				// Recover selection from a corrupt baseline bank without blaming
				// the identical signed code in the other bank.
				path := filepath.Join(l.r.slot(c.Active), "agentupdate")
				if e = os.Chmod(path, 0755); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, []byte("broken updater"), 0555); e != nil {
					t.Fatal(e)
				}
				l.offer = false
				if e = l.run("", "", 0); e != nil {
					t.Fatal(e)
				}
				repaired := l.load()
				if repaired.Active != c.Fallback || repaired.State != "committed" || repaired.Update.Rejected != 0 || repaired.Update.Result == "rollback" || repaired.Update.FirstFailure == nil || repaired.Update.Code != "hash_mismatch" || l.statuses[len(l.statuses)-1].Code != "hash_mismatch" {
					t.Fatal("baseline bank corruption blamed signed code or hid failure")
				}
				l.offer = true
			}
			if tc.kind == "prune" {
				for i := 0; i < 4; i++ {
					p := filepath.Join(dir, "slots", "a", strings.Repeat(fmt.Sprint(i), 64))
					os.MkdirAll(p, 0700)
					os.WriteFile(filepath.Join(p, "junk"), []byte("junk"), 0600)
				}
				os.MkdirAll(filepath.Join(dir, "slots", "b", ".partial-abandoned"), 0700)
			}
			err := l.run(tc.point, tc.mode, tc.skip)
			before := l.load()
			l.assertRunnable(before)
			if before.Floor < c.Floor {
				t.Fatal("floor decreased")
			}
			if tc.kind == "kill" && err == nil {
				t.Fatal("fault boundary was not exercised")
			}
			if tc.kind == "prepare-host-exit" {
				if err != nil || before.State != "committed" || before.Active.Sequence != 2 || before.Fallback.Sequence != 1 || before.Floor != 2 || before.Update.Rejected != 0 || before.Update.FirstFailure != nil || len(l.posts) != 1 || l.posts[0].Identity != before.Active.Identity {
					t.Fatal("higher signed offer could not repair broken baseline in both banks")
				}
				return
			}
			if tc.kind == "trial-host-errors" || tc.kind == "trial-fallback-exit" {
				if err != nil || before.State != "committed" || before.Active.Sequence != 2 || before.Floor != 2 || before.Update.Rejected != 0 || before.Confirmations != 0 || before.Update.FirstFailure != nil || len(before.Errors) != 1 || before.Errors[0] != "fixture\x00failed" || len(l.posts) != 1 || l.posts[0].Identity != before.Active.Identity {
					t.Fatal("valid candidate did not commit with the new host baseline")
				}
				return
			}
			if strings.HasPrefix(tc.kind, "trial-host-timeout") {
				if len(l.posts) != 0 {
					t.Fatal("timed-out candidate produced inventory")
				}
				if before.Confirmations != 1 || before.State != "prepared" || before.Candidate == nil || before.Active.Sequence != 1 || before.Floor != 2 || before.Update.Rejected != 0 || before.Update.FirstFailure == nil {
					t.Fatalf("host symptom consumed candidate: state=%s rejected=%d", before.State, before.Update.Rejected)
				}
				// Each invocation reloads the journal: retries must survive restarts.
				_ = l.run("", "", 0)
				retry := l.load()
				if retry.Confirmations != 2 || retry.State != "prepared" || retry.Update.Rejected != 0 {
					t.Fatal("second host symptom quarantined")
				}
				if strings.HasSuffix(tc.kind, "-bound") {
					_ = l.run("", "", 0)
					last := l.load()
					if last.Confirmations != 3 || last.State != "quarantined" || last.Update.Rejected != 2 || last.Floor != 2 || last.Active.Sequence != 1 {
						t.Fatal("host symptom retry budget unbounded")
					}
				} else {
					// Stop the host symptom without changing the signed candidate.
					// The recovery marker disables the fixture's host fault trigger.
					os.WriteFile(filepath.Join(dir, "host-recovered"), []byte("ok"), 0600)
					os.Remove(filepath.Join(dir, "host-symptom"))
					if e = l.run("", "", 0); e != nil {
						t.Fatal(e)
					}
					last := l.load()
					if last.State != "committed" || last.Active.Sequence != 2 || last.Floor != 2 || last.Update.Rejected != 0 || last.Update.FirstFailure != nil {
						t.Fatal("same candidate did not recover")
					}
				}
				return
			}
			if strings.HasPrefix(tc.kind, "committed-") {
				if before.State != "committed" || before.Active.Sequence != 2 {
					t.Fatal("trial did not commit")
				}
				symptom := "errors"
				switch tc.kind {
				case "committed-host-timeout", "committed-candidate-timeout":
					symptom = "timeout"
				case "committed-host-exit":
					symptom = "exit"
				case "committed-host-invalid":
					symptom = "malformed"
				case "committed-host-identity":
					symptom = "wrong-build"
				}
				if strings.HasPrefix(tc.kind, "committed-candidate-") {
					os.WriteFile(filepath.Join(dir, "break-collector"), []byte("break"), 0600)
				} else {
					os.WriteFile(filepath.Join(dir, "host-symptom"), []byte(symptom), 0600)
				}
				_ = l.run("", "", 0)
				failed := l.load()
				if tc.kind == "committed-candidate-timeout" {
					if failed.State != "quarantined" || failed.Active.Sequence != 1 || failed.Update.Rejected != 2 || failed.Floor != 2 {
						t.Fatal("candidate timeout not confirmed")
					}
					return
				}
				if failed.State != "committed" || failed.Active != before.Active || failed.Fallback != before.Fallback || failed.Floor != before.Floor || failed.FloorDigest != before.FloorDigest || failed.Update.Rejected != 0 {
					t.Fatal("host change rejected committed code or consumed floor")
				}
				if symptom == "errors" {
					if len(failed.Errors) != 1 || failed.Update.FirstFailure != nil {
						t.Fatal("committed diagnostic not recorded as baseline")
					}
				} else if failed.Update.FirstFailure == nil || len(l.statuses) == 0 || l.statuses[len(l.statuses)-1].Code == "" {
					t.Fatal("host execution failure not reported")
				}
				return
			}
			if tc.kind == "trial" || tc.kind == "worker" {
				if before.State != "quarantined" || before.Active.Sequence != 1 || before.Floor != 2 || before.Update.Rejected != 2 {
					t.Fatalf("bad code not quarantined: %+v", before)
				}
				first := *before.Update.FirstFailure
				for i := 0; i < 2; i++ {
					if e = l.run("", "", 0); e != nil {
						t.Fatal(e)
					}
				}
				after := l.load()
				if after.Update.FirstFailure == nil || *after.Update.FirstFailure != first || after.Floor != 2 || after.Active.Sequence != 1 {
					t.Fatal("rejected code replayed or first failure lost")
				}
				if tc.mode == "hung" {
					p, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
					var pid int
					fmt.Sscan(string(p), &pid)
					if pid == 0 || syscall.Kill(pid, 0) != syscall.ESRCH {
						t.Fatal("descendant not killed and reaped")
					}
				}
				l.publish(3, "good")
				if e = l.run("", "", 0); e != nil {
					t.Fatal(e)
				}
				after = l.load()
				if after.Active.Sequence != 3 || after.Fallback.Sequence != 1 || after.Update.FirstFailure != nil {
					t.Fatal("higher release failed repair")
				}
				return
			}
			switch tc.kind {
			case "current-delivery":
				l.mu.Lock()
				l.reject = true
				l.mu.Unlock()
				if l.run("", "", 0) == nil {
					t.Fatal("current delivery failure ignored")
				}
				failed := l.load()
				if failed.State != "committed" || failed.Update.FirstFailure == nil {
					t.Fatal("failure not retained")
				}
				l.mu.Lock()
				l.reject = false
				l.mu.Unlock()
				if e = l.run("", "", 0); e != nil {
					t.Fatal(e)
				}
				recovered := l.load()
				if recovered.Active.Sequence != 2 || recovered.Update.FirstFailure != nil || recovered.Update.Failures != 0 {
					t.Fatal("transient current failure stuck")
				}
				return
			case "partial", "signature", "hash", "platform", "roles", "traversal", "space", "inode-space", "journal-space", "held":
				if before.Floor != 1 || before.Active.Sequence != 1 {
					t.Fatal("preflight consumed floor")
				}
			case "mirror-space", "fsync":
				if before.Floor != 2 || before.Active.Sequence != 1 {
					t.Fatal("partial mirror lost floor")
				}
			case "rejected", "lost":
				if before.State != "report-pending" || before.Update.Rejected != 0 {
					t.Fatalf("delivery quarantined: %+v", before)
				}
				l.mu.Lock()
				l.reject = false
				l.mu.Unlock()
			case "later", "dead-updater":
				if before.Active.Sequence != 2 {
					t.Fatal("trial did not commit")
				}
				if tc.kind == "later" {
					os.WriteFile(filepath.Join(dir, "break-collector"), []byte("break"), 0600)
				} else {
					path := filepath.Join(l.r.slot(before.Active), "agentupdate")
					os.Chmod(path, 0755)
					os.WriteFile(path, []byte("broken updater"), 0555)
				}
				if tc.kind == "dead-updater" {
					// Offer the successor while the installed updater is still dead. Its
					// fetch and repair must originate in the launcher's independent path.
					l.publish(3, "good")
					if e = l.run("", "", 0); e != nil {
						t.Fatal(e)
					}
					repaired := l.load()
					if repaired.Active.Sequence != 3 || repaired.Fallback.Sequence != 1 || repaired.Floor != 3 {
						t.Fatal("independent repair failed")
					}
					return
				}
				if e = l.run("", "", 0); e != nil {
					t.Fatal(e)
				}
				failed := l.load()
				if failed.Active.Sequence != 1 || failed.Update.Rejected != 2 {
					t.Fatal("post-trial failure did not fall back")
				}
				l.publish(3, "good")
				if e = l.run("", "", 0); e != nil {
					t.Fatal(e)
				}
				repaired := l.load()
				if repaired.Active.Sequence != 3 || repaired.Fallback.Sequence != 1 || repaired.Floor != 3 {
					t.Fatal("launcher could not repair dead runtime")
				}
				return
			}
			l.mu.Lock()
			l.partial = false
			l.mu.Unlock()
			if tc.kind == "signature" || tc.kind == "hash" || tc.kind == "platform" || tc.kind == "roles" || tc.kind == "traversal" {
				l.publish(2, "good")
			}
			if e = l.run("", "", 0); e != nil {
				t.Fatal(e)
			}
			after := l.load()
			l.assertRunnable(after)
			if after.Floor < before.Floor {
				t.Fatal("restart lowered floor")
			}
			if before.State == "pending" || before.State == "quarantined" {
				if after.Active.Sequence != 1 || after.Update.Rejected != 2 {
					t.Fatal("ambiguous/rejected execution replayed")
				}
			}
			if tc.kind == "lost" {
				count := 0
				for _, rec := range l.posts {
					if rec.Nonce == before.Nonce {
						count++
					}
				}
				if count != 1 || after.Receipt == nil {
					t.Fatal("lost response was not idempotent")
				}
			}
			if tc.kind == "rejected" && after.Active.Sequence != 2 {
				t.Fatal("pending delivery did not recover")
			}
			if tc.kind == "prune" {
				for _, bank := range []string{"a", "b"} {
					entries, _ := os.ReadDir(filepath.Join(dir, "slots", bank))
					if len(entries) > 1 {
						t.Fatal("unbounded slots")
					}
				}
			}
			if e = l.run("", "", 0); e != nil {
				t.Fatal(e)
			}
			last := l.load()
			if last.Floor < after.Floor {
				t.Fatal("later floor decrease")
			}
		})
	}
}
