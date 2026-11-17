package plugin

import (
	"reflect"
	"testing"
	"time"
)

type packageFixture struct {
	fakePlugin
	source func() ([]PackageHost, error)
}

func (p *packageFixture) Manifest() Manifest {
	return Manifest{ID: "cve", Name: "Vulnerabilities", Version: "1"}
}
func (p *packageFixture) SetPackageSource(s func() ([]PackageHost, error)) { p.source = s }
func TestBindPackageSource(t *testing.T) {
	p := &packageFixture{}
	r, err := Open(t.TempDir(), []Builtin{{ID: "cve", New: func(h Host) Plugin { p.host = h; return p }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	want := []PackageHost{{Host: "example", CollectedAt: time.Now(), Sets: []PackageSet{{Scope: "host", Status: "ok", Packages: []Package{{Name: "binary", Source: "source", SourceVersion: "1"}}}}}}
	r.BindPackageSource(func() ([]PackageHost, error) { return want, nil })
	got, err := p.source()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	// It follows the lifecycle operation lock, just like network projections.
	e := r.entries["cve"]
	e.op.Lock()
	done := make(chan struct{})
	go func() { r.BindPackageSource(nil); close(done) }()
	select {
	case <-done:
		t.Fatal("binding bypassed lifecycle lock")
	case <-time.After(10 * time.Millisecond):
	}
	e.op.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("binding stuck")
	}
	if p.source != nil {
		t.Fatal("nil did not detach")
	}
	empty, err := Open(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	empty.BindPackageSource(nil)
}
