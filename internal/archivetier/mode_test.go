package archivetier

import (
	"context"
	"testing"
)

func TestModeChangesAndOldPolicyRefuseWrites(t *testing.T) {
	for _, attack := range []string{"mode", "version", "tag"} {
		t.Run(attack, func(t *testing.T) {
			r, a, _, now := runner(t)
			s := check(t, r, now)
			a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
			switch attack {
			case "mode":
				s.Job.Mode = Mode2160p
			case "version":
				s.Job.PolicyVersion = ""
			case "tag":
				a.opted = false
			}
			r.Store.Save(s)
			st, e := r.Check(context.Background(), now)
			if e != nil || st.Job.Phase != NeedsApproval || a.imports != 0 {
				t.Fatalf("unsafe import: %+v %v", st, e)
			}
		})
	}
}
