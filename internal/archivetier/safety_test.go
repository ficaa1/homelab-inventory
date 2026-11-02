package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeFS struct {
	entries map[string]Evidence
	volumes map[string]Volume
}

func (f *fakeFS) Stat(p string) (Evidence, error) {
	v, ok := f.entries[p]
	if !ok {
		return v, fmt.Errorf("missing")
	}
	return v, nil
}
func (f *fakeFS) Volume(p string) (Volume, error) {
	v, ok := f.volumes[p]
	if !ok {
		return v, fmt.Errorf("missing volume")
	}
	return v, nil
}
func (f *fakeFS) Walk(root string) ([]Evidence, error) {
	out := []Evidence{}
	for p, v := range f.entries {
		if under(p, root) && !v.Directory {
			out = append(out, v)
		}
	}
	return out, nil
}
func safetyClient(t *testing.T) (*Client, *fakeFS, *string, *string, *string) {
	t.Helper()
	version := "4.0.19.2979"
	recycle := "/library/recycle"
	script := "false"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("unexpected write")
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			json.NewEncoder(w).Encode(map[string]any{"appName": "Sonarr", "version": version})
		case "/api/v3/config/mediamanagement":
			fmt.Fprintf(w, `{"recycleBin":%q,"useScriptImport":%s}`, recycle, script)
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	c := NewClient("sonarr", server.URL, "fixture-key", true)
	c.Safety = testConfig()
	c.Safety.Mounts = []MountMapping{{"/library", "/readonly/library"}, {"/downloads", "/readonly/downloads"}}
	f := &fakeFS{entries: map[string]Evidence{}, volumes: map[string]Volume{}}
	add := func(p string, size int64, dev uint64, dir bool) {
		f.entries[p] = Evidence{Path: p, Size: size, Device: dev, Modified: 1, Directory: dir}
		f.volumes[p] = Volume{Device: dev, Free: 500 * GB, ReadOnly: true}
	}
	add("/readonly/library/recycle", 0, 1, true)
	add("/readonly/downloads", 0, 2, true)
	for _, v := range target().Files {
		add(strings.Replace(v.Path, "/library", "/readonly/library", 1), v.Size, 1, false)
	}
	for _, v := range fixture[[]ManualItem](t, "manual-sonarr.json") {
		add(strings.Replace(v.Path, "/downloads", "/readonly/downloads", 1), v.Size, 2, false)
	}
	c.FS = f
	return c, f, &version, &recycle, &script
}
func TestTargetPreflightAndRecovery(t *testing.T) {
	c, f, _, _, _ := safetyClient(t)
	ctx := context.Background()
	items := fixture[[]ManualItem](t, "manual-sonarr.json")
	// Identical names/sizes already present in recycle cannot prove this swap.
	p := "/readonly/library/recycle/preexisting/ep1.mkv"
	f.entries[p] = Evidence{Path: p, Size: target().Files[0].Size, Device: 1, Modified: 1}
	root, baseline, e := c.TargetPreflight(ctx, target(), items, 4*GB)
	if e != nil {
		t.Fatal(e)
	}
	j := &Job{Target: target(), RecycleRoot: root, RecycleBaseline: baseline}
	if _, e = c.Recovery(ctx, j); e == nil {
		t.Fatal("preexisting evidence accepted")
	}
	v := f.entries[p]
	v.Modified = 2
	f.entries[p] = v
	if _, e = c.Recovery(ctx, j); e == nil {
		t.Fatal("touched preexisting evidence accepted")
	}
	for _, v := range target().Files {
		p := "/readonly/library/recycle/new/" + filepath.Base(v.Path)
		f.entries[p] = Evidence{Path: p, Size: v.Size, Device: 1, Modified: 2}
	}
	j.Recovery, e = c.Recovery(ctx, j)
	if e != nil {
		t.Fatal(e)
	}
	before := len(f.entries)
	plan, e := RestorePlan(*j)
	if e != nil || len(plan) != 2 || plan[0].To != j.Target.Files[0].Path {
		t.Fatal(plan, e)
	}
	if len(f.entries) != before {
		t.Fatal("restore plan mutated filesystem")
	}
}
func TestSafetyFailsClosed(t *testing.T) {
	cases := []string{"version", "script", "script null", "missing recycle", "foreign recycle root", "cross device", "second file device", "space", "source same device", "source missing", "source size", "writable view", "unmapped", "overlapping map"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			c, f, version, recycle, script := safetyClient(t)
			items := fixture[[]ManualItem](t, "manual-sonarr.json")
			switch name {
			case "version":
				*version = "4.0.20.1"
			case "script":
				*script = "true"
			case "script null":
				*script = "null"
			case "missing recycle":
				*recycle = ""
			case "foreign recycle root":
				*recycle = "/downloads"
			case "cross device":
				st := f.entries["/readonly/library/recycle"]
				st.Device = 3
				f.entries[st.Path] = st
			case "second file device":
				st := f.entries["/readonly/library/Example Series/ep2.mkv"]
				st.Device = 3
				f.entries[st.Path] = st
				v := f.volumes[st.Path]
				v.Device = 3
				f.volumes[st.Path] = v
			case "space":
				v := f.volumes["/readonly/library/Example Series/ep1.mkv"]
				v.Free = 23 * GB
				f.volumes["/readonly/library/Example Series/ep1.mkv"] = v
			case "source same device":
				st := f.entries["/readonly/downloads"]
				st.Device = 1
				f.entries[st.Path] = st
				v := f.volumes[st.Path]
				v.Device = 1
				f.volumes[st.Path] = v
			case "source missing":
				delete(f.entries, "/readonly/downloads")
			case "source size":
				st := f.entries[strings.Replace(items[0].Path, "/downloads", "/readonly/downloads", 1)]
				st.Size--
				f.entries[st.Path] = st
			case "writable view":
				v := f.volumes["/readonly/library/recycle"]
				v.ReadOnly = false
				f.volumes["/readonly/library/recycle"] = v
			case "unmapped":
				c.Safety.Mounts = nil
			case "overlapping map":
				c.Safety.Mounts = append(c.Safety.Mounts, MountMapping{"/library/Example Series", "/readonly/library/Example Series"})
			}
			if _, _, e := c.TargetPreflight(context.Background(), target(), items, 4*GB); e == nil {
				t.Fatal("unsafe preflight passed")
			}
		})
	}
}
func TestRecoveryRejectsWrongSizeDeviceAndAmbiguity(t *testing.T) {
	for _, name := range []string{"wrong size", "foreign device", "ambiguous", "missing baseline", "changed recycle"} {
		t.Run(name, func(t *testing.T) {
			c, f, _, recycle, _ := safetyClient(t)
			root, base, e := c.TargetPreflight(context.Background(), target(), nil, 4*GB)
			if e != nil {
				t.Fatal(e)
			}
			j := &Job{Target: target(), RecycleRoot: root, RecycleBaseline: base}
			for _, v := range target().Files {
				p := "/readonly/library/recycle/new/" + filepath.Base(v.Path)
				f.entries[p] = Evidence{Path: p, Size: v.Size, Device: 1, Modified: 2}
			}
			p := "/readonly/library/recycle/new/ep1.mkv"
			st := f.entries[p]
			switch name {
			case "wrong size":
				st.Size--
				f.entries[p] = st
			case "foreign device":
				st.Device = 3
				f.entries[p] = st
			case "ambiguous":
				st.Path = "/readonly/library/recycle/other/ep1.mkv"
				f.entries[st.Path] = st
			case "missing baseline":
				j.RecycleBaseline = nil
			case "changed recycle":
				*recycle = "/downloads"
			}
			if _, e = c.Recovery(context.Background(), j); e == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
}
func TestLocalInspectionRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "file")
	alias := filepath.Join(root, "alias")
	if e := os.WriteFile(real, []byte("fixture"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(real, alias); e != nil {
		t.Fatal(e)
	}
	fs := LocalFilesystem{}
	if _, e := fs.Stat(real); e != nil {
		t.Fatal(e)
	}
	if _, e := fs.Stat(alias); e == nil {
		t.Fatal("symlink allowed")
	}
	if _, e := fs.Walk(root); e == nil {
		t.Fatal("symlink recycle tree accepted")
	}
}

func TestRecycleBaselineMergeKeepsOriginalEvidence(t *testing.T) {
	root := "/library/recycle"
	before := []Evidence{{Path: root, Device: 1, Directory: true}, {Path: root + "/ep.mkv", Size: 10, Device: 1, Modified: 1}}
	now := []Evidence{{Path: root, Device: 1, Directory: true}, {Path: root + "/ep.mkv", Size: 10, Device: 1, Modified: 2}, {Path: root + "/new.mkv", Size: 11, Device: 1}}
	merged, e := mergeBaseline(root, before, now)
	if e != nil || len(merged) != 3 || merged[1].Modified != 1 {
		t.Fatal("baseline changed/lost", merged, e)
	}
	now[0].Device = 2
	if _, e := mergeBaseline(root, before, now); e == nil {
		t.Fatal("changed root device accepted")
	}
	if _, e := mergeBaseline(root, nil, now); e == nil {
		t.Fatal("missing original device accepted")
	}
}
func TestRestorePlanRefusesIncompleteAndAmbiguousEvidence(t *testing.T) {
	if _, e := RestorePlan(Job{}); e == nil {
		t.Fatal("empty recovery accepted")
	}
	job := Job{Target: target(), RecycleRoot: "/library/recycle", Recovery: []Evidence{{Path: "/library/recycle/ep1.mkv", Size: 50 * GB}, {Path: "/library/recycle/ep2.mkv", Size: 51 * GB}}}
	if _, e := RestorePlan(job); e != nil {
		t.Fatal(e)
	}
	job.Recovery[1].Size--
	if _, e := RestorePlan(job); e == nil {
		t.Fatal("wrong-size recovery accepted")
	}
	job.Recovery = job.Recovery[:1]
	if _, e := RestorePlan(job); e == nil {
		t.Fatal("partial recovery accepted")
	}
}
