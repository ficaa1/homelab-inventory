package agentupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if e := os.WriteFile(path, data, 0755); e != nil {
		t.Fatal(e)
	}
}
func setup(t *testing.T) (Runner, State, agentrelease.Manifest, map[string][]byte) {
	t.Helper()
	r := Runner{Dir: t.TempDir(), Token: "test", Host: "host-a"}
	files := map[string][]byte{}
	inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: "new", Revision: 5}}
	data, _ := json.Marshal(inv)
	files["discover-"+platform()] = append([]byte("#!/bin/sh\nif [ \"$1\" = -list ]; then exit 0; fi\ncat <<'JSON'\n"), append(data, []byte("\nJSON\n")...)...)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(201)
		}
	}))
	t.Cleanup(api.Close)
	r.URL = api.URL
	m := agentrelease.Manifest{Platforms: []string{"linux-amd64", "linux-arm64", "freebsd-amd64"}, Sequence: 2, Revision: 5, Version: "new", Created: time.Now(), Artifacts: map[string]agentrelease.Artifact{}}
	for _, c := range []string{"discover"} {
		name := c + "-" + platform()
		b := files[name]
		m.Artifacts[platform()+"/"+c] = agentrelease.Artifact{Name: name, SHA256: agentrelease.Digest(b), Size: int64(len(b))}
	}
	old := m
	old.Sequence = 1
	if e := r.Stage(context.Background(), old, func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }); e != nil {
		t.Fatal(e)
	}
	s := State{High: 1, Current: 1, Update: inventory.AgentUpdate{Sequence: 1}}
	if e := r.save(s); e != nil {
		t.Fatal(e)
	}
	if e := r.point(1); e != nil {
		t.Fatal(e)
	}
	return r, s, m, files
}
func TestAtomicSwapRollbackAndReplay(t *testing.T) {
	r, s, m, files := setup(t)
	fetch := func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }
	if e := r.Stage(context.Background(), m, fetch); e != nil {
		t.Fatal(e)
	}
	if e := r.Activate(&s, m); e != nil {
		t.Fatal(e)
	}
	current, _ := os.Readlink(filepath.Join(r.Dir, "current"))
	if current != "releases/2" || !s.Pending {
		t.Fatal(current, s)
	}
	if e := r.Rollback(&s); e != nil {
		t.Fatal(e)
	}
	current, _ = os.Readlink(filepath.Join(r.Dir, "current"))
	if current != "releases/1" || s.High != 2 || s.Pending {
		t.Fatal(current, s)
	}
	for _, seq := range []uint64{1, 2} {
		m.Sequence = seq
		if e := r.Activate(&s, m); e == nil {
			t.Fatal("replay/downgrade accepted")
		}
	}
}
func TestCorruptDownloadLeavesCurrent(t *testing.T) {
	r, s, m, _ := setup(t)
	if e := r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return []byte("corrupt"), nil }); e == nil {
		t.Fatal("corrupt accepted")
	}
	current, _ := os.Readlink(filepath.Join(r.Dir, "current"))
	if current != "releases/1" || s.High != 1 {
		t.Fatal("active bundle changed")
	}
}
func TestFailedJournalLeavesCurrent(t *testing.T) {
	r, s, m, _ := setup(t)
	os.Remove(filepath.Join(r.Dir, "state.json"))
	os.Mkdir(filepath.Join(r.Dir, "state.json"), 0700)
	if e := r.Activate(&s, m); e == nil {
		t.Fatal("state failure ignored")
	}
	current, _ := os.Readlink(filepath.Join(r.Dir, "current"))
	if current != "releases/1" || s.Current != 1 {
		t.Fatal("state failure mutated active bundle")
	}
}
func TestInterruptedTrialRollsBackDespiteAPIOutage(t *testing.T) {
	r, s, m, files := setup(t)
	if e := r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }); e != nil {
		t.Fatal(e)
	}
	if e := r.Activate(&s, m); e != nil {
		t.Fatal(e)
	}
	s.TrialStarted = true
	if e := r.save(s); e != nil {
		t.Fatal(e)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer api.Close()
	r.URL = api.URL
	if e := r.Run(context.Background()); e == nil {
		t.Fatal("outage reported success")
	}
	s, e := r.load()
	if e != nil || s.Current != 1 || s.High != 2 || s.Update.Failures == 0 {
		t.Fatal(s, e)
	}
}
func TestNextReportFailureRollsBack(t *testing.T) {
	r, s, m, files := setup(t)
	files["discover-"+platform()] = []byte("#!/bin/sh\nif [ \"$1\" = -list ]; then exit 0; fi\nexit 1\n")
	a := m.Artifacts[platform()+"/discover"]
	a.SHA256 = agentrelease.Digest(files[a.Name])
	a.Size = int64(len(files[a.Name]))
	m.Artifacts[platform()+"/discover"] = a
	if e := r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }); e != nil {
		t.Fatal(e)
	}
	if e := r.Activate(&s, m); e != nil {
		t.Fatal(e)
	}
	if e := r.report(context.Background(), s); e == nil {
		t.Fatal("bad report succeeded")
	}
	if e := r.Rollback(&s); e != nil {
		t.Fatal(e)
	}
	if e := r.report(context.Background(), s); e != nil {
		t.Fatal("old bundle failed", e)
	}
}
func TestReportPostsCompletePartialInventory(t *testing.T) {
	r, s, _, _ := setup(t)
	inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: "new", Revision: 5}, Diagnostics: []inventory.Diagnostic{{Severity: inventory.SeverityError}}}
	data, _ := json.Marshal(inv)
	write(t, filepath.Join(r.bundle(1), "discover"), append([]byte("#!/bin/sh\ncat <<'JSON'\n"), append(data, []byte("\nJSON\n")...)...))
	posts := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var got inventory.Inventory
		if req.Method != "POST" || req.Header.Get("Authorization") != "Bearer test" || json.NewDecoder(req.Body).Decode(&got) != nil || got.AgentUpdate == nil {
			t.Error("bad report")
		}
		posts++
		w.WriteHeader(201)
	}))
	defer api.Close()
	r.URL = api.URL
	s.Pending = false
	if e := r.report(context.Background(), s); e != nil {
		t.Fatal("ordinary partial inventory refused", e)
	}
}

func TestReportKeepsFieldsUnknownToUpdater(t *testing.T) {
	r, s, _, _ := setup(t)
	data, _ := json.Marshal(inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: "new", Revision: 5}})
	data = append(data[:len(data)-1], []byte(`,"future_field":{"n":1}}`)...)
	write(t, filepath.Join(r.bundle(1), "discover"), append([]byte("#!/bin/sh\ncat <<'JSON'\n"), append(data, []byte("\nJSON\n")...)...))
	var got map[string]json.RawMessage
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewDecoder(req.Body).Decode(&got)
		w.WriteHeader(201)
	}))
	defer api.Close()
	r.URL = api.URL
	if e := r.report(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if string(got["future_field"]) != `{"n":1}` || got["agent_update"] == nil {
		t.Fatal("unknown field dropped or status missing", got)
	}
}

func TestReportRejectsFoldedDuplicateKeys(t *testing.T) {
	for _, extra := range []string{`"HOST":{"hostname":"evil"}`, `"hoſt":{"hostname":"evil"}`, `"Schema_Version":"0"`, `"schema_verſion":"0"`, `"Agent_Update":{"state":"x"}`} {
		r, s, _, _ := setup(t)
		data, _ := json.Marshal(inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: "new", Revision: 5}})
		data = append(data[:len(data)-1], []byte(","+extra+"}")...)
		write(t, filepath.Join(r.bundle(1), "discover"), append([]byte("#!/bin/sh\ncat <<'JSON'\n"), append(data, []byte("\nJSON\n")...)...))
		posts := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { posts++; w.WriteHeader(201) }))
		r.URL = api.URL
		if e := r.report(context.Background(), s); e == nil || posts != 0 {
			t.Errorf("%s: accepted or posted", extra)
		}
		api.Close()
	}
}

func TestRecoveryPointerFailureStillReportsPrevious(t *testing.T) {
	r, s, m, files := setup(t)
	if e := r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }); e != nil {
		t.Fatal(e)
	}
	if e := r.Activate(&s, m); e != nil {
		t.Fatal(e)
	}
	s.TrialStarted = true
	if e := r.save(s); e != nil {
		t.Fatal(e)
	}
	// Simulate a filesystem refusing the recovery rename (as ENOSPC can).
	if e := os.Remove(filepath.Join(r.Dir, "current")); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(r.Dir, "current"), 0700); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(r.Dir, "current", "occupied"), []byte("marker"))
	// The new report fails. Successful Run must therefore use the old discover.
	write(t, filepath.Join(r.bundle(2), "discover"), []byte("#!/bin/sh\nexit 1\n"))
	if e := r.Run(context.Background()); e != nil {
		t.Fatal("old report lost to recovery write failure", e)
	}
}

// Exercise the supervisor, rather than manually calling Rollback as the original
// trial test did. This build passed its trial but breaks on a later invocation.
func TestCommittedBuildFailureAutomaticallyRecovers(t *testing.T) {
	r, s, m, files := setup(t)
	if e := r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return files[a.Name], nil }); e != nil {
		t.Fatal(e)
	}
	if e := r.Activate(&s, m); e != nil {
		t.Fatal(e)
	}
	s.Pending = false
	if e := r.save(s); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(r.bundle(2), "discover"), []byte("#!/bin/sh\nexit 1\n"))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer api.Close()
	r.URL = api.URL
	if e := r.Run(context.Background()); e != nil {
		t.Fatal("automatic recovery lost reporting", e)
	}
	got, e := r.load()
	if e != nil || got.Current != 1 || got.High != 2 || got.Pending || got.Update.Result != "rollback" {
		t.Fatal(got, e)
	}
	target, _ := os.Readlink(filepath.Join(r.Dir, "current"))
	if target != "releases/1" {
		t.Fatal(target)
	}
	// A failing fallback must stop; it cannot roll back to itself every run.
	write(t, filepath.Join(r.bundle(1), "discover"), []byte("#!/bin/sh\nexit 1\n"))
	if e := r.Run(context.Background()); e == nil {
		t.Fatal("broken fallback reported success")
	}
	if e := r.Recover(); e == nil {
		t.Fatal("rollback to itself accepted")
	}
	got, _ = r.load()
	if got.High != 2 || got.Current != 1 {
		t.Fatal("recovery loop changed replay state", got)
	}
}

func signedOffer(t *testing.T, r *Runner, m agentrelease.Manifest, files map[string][]byte, posted *[]uint64) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r.Public = filepath.Join(r.Dir, "public.key")
	if err = os.WriteFile(r.Public, []byte(base64.StdEncoding.EncodeToString(pub)), 0644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		name := "discover-" + target
		if _, ok := files[name]; !ok {
			files[name] = files["discover-"+platform()]
		}
		m.Artifacts[target+"/discover"] = agentrelease.Artifact{Name: name, SHA256: agentrelease.Digest(files[name]), Size: int64(len(files[name]))}
	}
	payload, _ := json.Marshal(m)
	envelope, _ := json.Marshal(agentrelease.Signed{Manifest: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/agent/status" {
			w.WriteHeader(204)
			return
		}
		if req.URL.Path == "/agent/manifest" {
			w.Write(envelope)
			return
		}
		if req.Method == "GET" {
			w.Write(files[filepath.Base(req.URL.Path)])
			return
		}
		var inv inventory.Inventory
		if json.NewDecoder(req.Body).Decode(&inv) != nil || inv.AgentUpdate == nil {
			t.Error("invalid inventory posted")
			w.WriteHeader(400)
			return
		}
		*posted = append(*posted, inv.AgentUpdate.Sequence)
		w.WriteHeader(201)
	}))
	t.Cleanup(api.Close)
	r.URL = api.URL
}

func TestSupervisorRejectsSilentMalformedAndOversizedBuilds(t *testing.T) {
	for _, body := range []string{"exit 0", "printf '{'", "head -c 16777217 /dev/zero", "echo '{\"schema_version\":\"5\",\"host\":{\"hostname\":\"host-a\"},\"tool\":{\"version\":\"wrong\",\"revision\":5}}'"} {
		t.Run(body[:min(len(body), 12)], func(t *testing.T) {
			r, _, m, files := setup(t)
			files["discover-"+platform()] = []byte("#!/bin/sh\nif [ \"$1\" = -list ]; then exit 0; fi\n" + body + "\n")
			var posted []uint64
			signedOffer(t, &r, m, files, &posted)
			if err := r.Run(context.Background()); err != nil {
				t.Fatal("fallback did not report", err)
			}
			s, err := r.load()
			if err != nil || s.Current != 1 || s.High != 2 || s.Pending {
				t.Fatal("invalid trial committed", s, err)
			}
			if len(posted) != 1 || posted[0] != 1 {
				t.Fatal("only previous build should report", posted)
			}
			if err := r.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			s, _ = r.load()
			if s.High != 2 || s.Current != 1 || len(posted) != 2 || posted[1] != 1 {
				t.Fatal("bad release replayed", s, posted)
			}
		})
	}
}

func TestSupervisorCommitsOnlyAfterPostingAndRecoversNextRun(t *testing.T) {
	r, _, m, files := setup(t)
	var posted []uint64
	signedOffer(t, &r, m, files, &posted)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := r.load()
	if err != nil || s.Current != 2 || s.High != 2 || s.Pending || len(posted) != 1 || posted[0] != 2 {
		t.Fatal(s, err, posted)
	}
	write(t, filepath.Join(r.bundle(2), "discover"), []byte("#!/bin/sh\nexit 1\n"))
	if err := r.Run(context.Background()); err != nil {
		t.Fatal("later failure lost recovery", err)
	}
	s, _ = r.load()
	if s.Current != 1 || s.High != 2 || s.Pending || len(posted) != 2 || posted[1] != 1 {
		t.Fatal(s, posted)
	}
}

func TestOutputLimitAppliesDuringCopy(t *testing.T) {
	var out cappedOutput
	// LimitReader prevents WriterTo so Copy would use a promoted Buffer.ReadFrom
	// if the capped writer accidentally embedded bytes.Buffer.
	_, err := io.Copy(&out, io.LimitReader(strings.NewReader(strings.Repeat("x", (16<<20)+1)), (16<<20)+1))
	if err == nil || len(out.Bytes()) > 16<<20 {
		t.Fatal("output limit bypassed", len(out.Bytes()), err)
	}
}

func TestRetryQuarantineMigrationAndDiagnosticBaseline(t *testing.T) {
	ctx := context.Background()
	t.Run("download and disk failure retry without reservation", func(t *testing.T) {
		for _, code := range []error{fault("download", "network_unreachable", nil), syscall.ENOSPC} {
			r, s, m, files := setup(t)
			for i := 0; i < 2; i++ {
				err := r.Stage(ctx, m, func(agentrelease.Artifact) ([]byte, error) { return nil, code })
				if err == nil || s.High != 1 {
					t.Fatal(s, err)
				}
			}
			var posts []uint64
			signedOffer(t, &r, m, files, &posts)
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			s, _ = r.load()
			if s.Current != 2 {
				t.Fatal(s)
			}
		}
	})

	t.Run("download retry and healthy trial delivery retry", func(t *testing.T) {
		for _, path := range []string{"/agent/artifacts/", "/inventory"} {
			r, _, m, files := setup(t)
			var posts []uint64
			signedOffer(t, &r, m, files, &posts)
			transport := &failOnceTransport{path: path}
			r.Client = &http.Client{Transport: transport}
			err := r.Run(ctx)
			s, _ := r.load()
			if path == "/agent/artifacts/" {
				if err != nil || s.High != 1 || s.Current != 1 || s.Update.LastFailure.Stage != "download" {
					t.Fatal(s, err)
				}
			} else if err == nil || !s.Pending || !s.TrialHealthy || s.Update.Rejected != 0 {
				t.Fatal(s, err)
			}
			if err = r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			s, _ = r.load()
			if s.Current != 2 || s.Pending || s.Update.Rejected != 0 || s.Update.FirstFailure != nil || transport.calls != 2 {
				t.Fatal(s, transport.calls)
			}
			// Same sequence with different signed bytes cannot silently become current.
			m.Version = "different"
			signedOffer(t, &r, m, files, &posts)
			r.Client = nil
			if err = r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			s, _ = r.load()
			if s.Current != 2 || s.Update.Code != "manifest_conflict" {
				t.Fatal(s)
			}
		}
	})
	t.Run("rejected stays quiet and higher release repairs", func(t *testing.T) {
		r, _, m, files := setup(t)
		original := files["discover-"+platform()]
		files["discover-"+platform()] = []byte("#!/bin/sh\nif [ \"$1\" = -list ]; then exit 0; fi\nexit 2\n")
		var posts []uint64
		signedOffer(t, &r, m, files, &posts)
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
		s, _ := r.load()
		first := s.Update.FirstFailure
		count := s.Update.Failures
		for i := 0; i < 3; i++ {
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
		}
		s, _ = r.load()
		if s.Update.Rejected != 2 || s.Update.Result != "blocked" || s.Update.Failures != count || s.Update.FirstFailure.Code != first.Code || !s.Update.FirstFailure.At.Equal(first.At) {
			t.Fatal(s)
		}
		files = map[string][]byte{"discover-" + platform(): original}
		m.Sequence = 3
		signedOffer(t, &r, m, files, &posts)
		if err := r.Run(ctx); err != nil {
			t.Fatal(err)
		}
		s, _ = r.load()
		if s.Current != 3 || s.High != 3 || s.Update.FirstFailure != nil || s.Update.Rejected != 0 {
			t.Fatal(s)
		}
	})
	t.Run("old replay trap migrates once", func(t *testing.T) {
		r := Runner{Dir: t.TempDir()}
		write(t, filepath.Join(r.Dir, "state.json"), []byte(`{"high":6,"current":5,"update":{"sequence":5,"result":"failed","failures":466}}`))
		s, err := r.load()
		if err != nil || s.Update.Rejected != 6 || s.Update.FirstFailure.Code != "unknown" || s.Update.Result != "blocked" {
			t.Fatal(s, err)
		}
		again, err := r.load()
		if err != nil || again.Update.Failures != s.Update.Failures {
			t.Fatal(again, err)
		}
	})
	for _, newError := range []bool{false, true} {
		t.Run(fmt.Sprintf("diagnostic regression %v", newError), func(t *testing.T) {
			r, s, m, files := setup(t)
			inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: "new", Revision: 5}, Diagnostics: []inventory.Diagnostic{{Collector: "storage", Code: "unavailable", Severity: inventory.SeverityError, Message: "ignored local text"}}}
			script := func(inv inventory.Inventory) []byte {
				b, _ := json.Marshal(inv)
				return []byte("#!/bin/sh\nif [ \"$1\" = -list ]; then exit 0; fi\ncat <<'JSON'\n" + string(b) + "\nJSON\n")
			}
			write(t, filepath.Join(r.bundle(s.Current), "discover"), script(inv))
			if newError {
				inv.Diagnostics = append(inv.Diagnostics, inventory.Diagnostic{Collector: "network", Code: "failed", Severity: inventory.SeverityError})
			}
			files["discover-"+platform()] = script(inv)
			var posts []uint64
			signedOffer(t, &r, m, files, &posts)
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			s, _ = r.load()
			if newError {
				if s.Update.Rejected != 2 || s.Update.FirstFailure.Code != "trial_new_errors" {
					t.Fatal(s)
				}
			} else if s.Current != 2 || s.Pending {
				t.Fatal(s)
			}
		})
	}
}

type failOnceTransport struct {
	path  string
	calls int
}

func (f *failOnceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.URL.Path, f.path) {
		f.calls++
		if f.calls == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}
