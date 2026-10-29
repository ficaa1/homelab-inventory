package settings

import (
	"github.com/ficaa1/homelab-inventory/internal/store"
	"testing"
	"time"
)

func TestRetentionSettingsValidationAndLayering(t *testing.T) {
	ptr := func(n int) *int { return &n }
	for _, u := range []Retention{
		{KeepAllDays: ptr(-1)}, {HourlyDays: ptr(-1)}, {DailyDays: ptr(-1)}, {CompressAfterDays: ptr(-1)},
		{KeepAllDays: ptr(31)}, {HourlyDays: ptr(366)}, {DailyDays: ptr(29)},
	} {
		if _, err := Apply(Stored{}, Update{RunRetention: &u}, time.Now()); err == nil {
			t.Fatalf("accepted %+v", u)
		}
	}
	seed := store.Policy{KeepAllDays: 10, HourlyDays: 50, DailyDays: 500, CompressAfterDays: 4}
	if _, err := Apply(Stored{}, Update{RunRetention: &Retention{HourlyDays: ptr(8)}}, time.Now(), Defaults{RunRetention: &seed}); err == nil {
		t.Fatal("did not validate merged env defaults")
	}
	st, err := Apply(Stored{}, Update{RunRetention: &Retention{DailyDays: ptr(0), CompressAfterDays: ptr(0)}}, time.Now(), Defaults{RunRetention: &seed})
	if err != nil {
		t.Fatal(err)
	}
	eff := Resolve(st, Defaults{RunRetention: &seed}).RunRetention
	if eff != (store.Policy{KeepAllDays: 10, HourlyDays: 50, DailyDays: 0, CompressAfterDays: 0}) {
		t.Fatalf("unset/zero lost: %+v", eff)
	}
	next, err := Apply(st, Update{RunRetention: &Retention{KeepAllDays: ptr(2)}}, time.Now(), Defaults{RunRetention: &seed})
	if err != nil {
		t.Fatal(err)
	}
	if st.RunRetention.KeepAllDays != nil {
		t.Fatal("mutated original")
	}
	if Resolve(next, Defaults{RunRetention: &seed}).RunRetention != (store.Policy{KeepAllDays: 2, HourlyDays: 50, DailyDays: 0, CompressAfterDays: 0}) {
		t.Fatal("partial update reset fields")
	}
}
