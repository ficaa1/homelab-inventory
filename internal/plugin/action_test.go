package plugin

import (
	"slices"
	"strings"
	"testing"
)

func TestViewActionInstances(t *testing.T) {
	a := ActionSpec{ID: "set_mode", Label: "Set mode", Operator: true, Result: "toast", Fields: []Field{
		{Key: "mode", Type: Select, Options: []Option{{"user", "User"}, {"model", "Model"}}},
		{Key: "key", Type: Secret}, {Key: "enabled", Type: Bool}, {Key: "count", Type: Number},
	}}
	m := Manifest{ID: "example", Name: "Example", Actions: []ActionSpec{a}}
	if err := validateManifest(m); err != nil {
		t.Fatal(err)
	}
	instance := a
	instance.Fields = slices.Clone(a.Fields)
	instance.Fields[0].Default = "model"
	instance.Label = "Set example mode"
	instance.Confirm = strings.Repeat("界", 300)
	instance.Values = map[string]any{"enabled": false, "count": "5"}
	check := func(ok bool, actions ...ActionSpec) {
		t.Helper()
		_, err := ValidateView(View{Actions: actions}, m)
		if (err == nil) != ok {
			t.Fatalf("unexpected validation result: %v", err)
		}
	}
	check(true, instance, a)
	check(false, slices.Repeat([]ActionSpec{instance}, 51)...)
	for _, change := range []func(*ActionSpec){
		func(v *ActionSpec) { v.Operator = false }, func(v *ActionSpec) { v.Destructive = true }, func(v *ActionSpec) { v.TimeoutSeconds = 1 }, func(v *ActionSpec) { v.Result = "view" }, func(v *ActionSpec) { v.Result = "other" },
		func(v *ActionSpec) { v.Fields[0].Required = true }, func(v *ActionSpec) { v.Fields[0].Default = "other" }, func(v *ActionSpec) { v.Fields[1].Default = "fixture" },
		func(v *ActionSpec) { v.Confirm = "unsafe<" }, func(v *ActionSpec) { v.Confirm += "界" }, func(v *ActionSpec) { v.Values = Config{"key": "fixture"} }, func(v *ActionSpec) { v.Values = Config{"count": false} }, func(v *ActionSpec) { v.Values = Config{"mode": "other"} }, func(v *ActionSpec) { v.Values = Config{"unknown": "name"} },
	} {
		v := instance
		v.Fields = slices.Clone(instance.Fields)
		change(&v)
		check(false, v)
	}
}
