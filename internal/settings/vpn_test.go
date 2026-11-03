package settings

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestVPNAutofixSettings(t *testing.T) {
	t.Parallel()
	base := Stored{}
	if got := Resolve(base, defaults()); got.VPNAutofix || got.VPNAutofixAfterMinutes != 10 {
		t.Fatalf("unsafe defaults: autofix=%v after=%d", got.VPNAutofix, got.VPNAutofixAfterMinutes)
	}
	enabled, after := true, 20
	saved, err := Apply(base, Update{VPNAutofix: &enabled, VPNAutofixAfterMinutes: &after}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The stored update owns its values, independent of its caller.
	enabled, after = false, 1
	blob, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Stored
	if err := json.Unmarshal(blob, &loaded); err != nil {
		t.Fatal(err)
	}
	got := Resolve(loaded, defaults())
	if !got.VPNAutofix || got.VPNAutofixAfterMinutes != 20 {
		t.Fatalf("lost persisted values: %+v", got)
	}
	changed, err := Apply(loaded, Update{VPNAutofix: &enabled}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got = Resolve(changed, defaults())
	if got.VPNAutofix || got.VPNAutofixAfterMinutes != 20 {
		t.Fatal("explicit false or partial update lost")
	}
	for _, minutes := range []int{-1, 0, 1441} {
		invalid, err := Apply(loaded, Update{VPNAutofix: &enabled, VPNAutofixAfterMinutes: &minutes}, time.Now())
		if err == nil || !reflect.DeepEqual(invalid, loaded) {
			t.Fatalf("non-atomic invalid delay %d", minutes)
		}
	}
	names := []string{" gerbil", "gerbil", "", "gluetun"}
	skipped, err := Apply(loaded, Update{VPNAutofixSkip: &names}, time.Now())
	if err != nil || !reflect.DeepEqual(Resolve(skipped, defaults()).VPNAutofixSkip, []string{"gerbil", "gluetun"}) {
		t.Fatal("skip list not cleaned", err)
	}
	bad := []string{"gerbil", "a b"}
	if invalid, err := Apply(loaded, Update{VPNAutofixSkip: &bad}, time.Now()); err == nil || !reflect.DeepEqual(invalid, loaded) {
		t.Fatal("invalid gateway name accepted")
	}
	for _, minutes := range []int{1, 1440} {
		if _, err := Apply(base, Update{VPNAutofixAfterMinutes: &minutes}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}
