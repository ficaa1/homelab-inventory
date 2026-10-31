package main

import (
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/settings"
)

func TestMergeBackupDefaultsPreservesNewDestinationFields(t *testing.T) {
	base := settings.Defaults{
		NotifyLevel:    alert.LevelWarning,
		NotifyInterval: 5 * time.Minute,
		DigestInterval: 24 * time.Hour,
		DashboardName:  "homelab",
		IconsFetch:     true,
	}
	source := `{"NotifyLevel":"critical","NotifyInterval":300000000000}`
	got, err := mergeBackupDefaults(base, source)
	if err != nil {
		t.Fatal(err)
	}
	if got.NotifyLevel != alert.LevelCritical || got.NotifyInterval != 5*time.Minute {
		t.Fatalf("source settings did not overlay: %+v", got)
	}
	if got.DashboardName != "homelab" || !got.IconsFetch {
		t.Fatalf("new destination defaults were erased: %+v", got)
	}
}
