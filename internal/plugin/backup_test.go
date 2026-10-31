package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigratedDefaultsKeepExplicitNull(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state-migration"), 0700)
	raw, _ := json.Marshal(map[string]Config{"sample": {"endpoint": "http://fixture.invalid", "key": "source-secret", "count": 5.0}})
	b, _ := json.Marshal(map[string]string{"APP_PLUGINS_EFFECTIVE": string(raw)})
	os.WriteFile(filepath.Join(dir, "state-migration", "source-config.json"), b, 0600)
	p := &fakePlugin{}
	r, e := Open(dir, []Builtin{{ID: "sample", New: func(h Host) Plugin { p.host = h; return p }}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	on := true
	if _, e = r.Update("sample", Update{Enabled: &on, Settings: Config{"key": nil}}); e != nil {
		t.Fatal(e)
	}
	if p.config["endpoint"] != "http://fixture.invalid" || p.config["count"] != 5.0 {
		t.Fatal("source defaults missing")
	}
	if _, ok := p.config["key"]; ok {
		t.Fatal("null tombstone revived secret")
	}
	if _, e = r.Test(context.Background(), "sample", Config{}); e != nil {
		t.Fatal(e)
	}
	if _, ok := p.config["key"]; ok {
		t.Fatal("test revived secret")
	}
}
