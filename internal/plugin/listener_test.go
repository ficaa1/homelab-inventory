package plugin

import "testing"

type listenerFixture struct {
	fakePlugin
	starts int
}

func (p *listenerFixture) Start() error { p.starts++; return nil }
func TestListenerActivationExcludesPreviewAndDisabled(t *testing.T) {
	p := &listenerFixture{}
	dir := t.TempDir()
	open := func() *Registry {
		r, e := Open(dir, []Builtin{{ID: "sample", New: func(h Host) Plugin { p.host = h; return p }}}, nil)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	r := open()
	if _, e := r.Update("sample", enabledUpdate()); e != nil {
		t.Fatal(e)
	}
	if p.starts != 0 {
		t.Fatal("staged update started listener")
	}
	r.Close()
	r = open()
	defer r.Close()
	if p.starts != 0 {
		t.Fatal("preview Open started listener")
	}
	r.activate()
	if p.starts != 1 {
		t.Fatal("runtime did not start listener")
	}
	off := false
	if _, e := r.Update("sample", Update{Enabled: &off}); e != nil {
		t.Fatal(e)
	}
	before := p.closed
	r.deactivate()
	if p.closed <= before {
		t.Fatal("shutdown did not close listener")
	}
	beforeStart := p.starts
	r.activate()
	if p.starts != beforeStart {
		t.Fatal("disabled listener started")
	}
}
