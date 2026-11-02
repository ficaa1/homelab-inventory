package main

import (
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// The shipped examples must load with unknown fields refused and validate.
func TestExampleConfigsLoad(t *testing.T) {
	for _, p := range []string{"../../deploy/mediapolicy/policy.example.json"} {
		c, err := readConfig(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		c.ArchiveEnabled, c.Seeding.Enabled = true, true
		if err := c.Validate(); err != nil {
			t.Errorf("enabled workers: %v", err)
		}
	}
}

func TestRestoreSelectsFailedListWithoutLosingActiveJob(t *testing.T) {
	active := &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 3}, Phase: archivetier.Downloading}
	failed := &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 4}, Phase: archivetier.Failed}
	other := &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 5}, Phase: archivetier.Failed}
	st := archivetier.State{Job: active, FailedJobs: []*archivetier.Job{failed}}
	if j, err := restoreJob(st, ""); err != nil || j != failed {
		t.Fatal("single failure unavailable", j, err)
	}
	st.FailedJobs = append(st.FailedJobs, other)
	if _, err := restoreJob(st, ""); err == nil {
		t.Fatal("ambiguous restore selected a job")
	}
	if j, err := restoreJob(st, archivetier.JobID(*other)); err != nil || j != other {
		t.Fatal("selected failure unavailable", j, err)
	}
}
