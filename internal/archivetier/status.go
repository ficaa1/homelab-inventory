package archivetier

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

// StatusHandler serves an authenticated, uncached snapshot for dashboard collectors.
func StatusHandler(dir, token string, maxAge time.Duration, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !httpx.BearerOK(req, token, httpx.DenyWhenUnset) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if req.URL.Path != "/status" && req.URL.Path != "/healthz" {
			http.NotFound(w, req)
			return
		}
		st, e := ReadState(dir)
		st = PublicState(st)
		stale := st.Checked.IsZero() || now().Sub(st.Checked) > maxAge
		healthy := Healthy(st) && e == nil && !stale
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if req.URL.Path == "/healthz" && !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(map[string]any{"healthy": healthy, "stale": stale, "state": st})
	})
}

// Healthy also checks persisted failure phases so clearing a transient error cannot
// hide an unresolved replacement across restarts or dry-run checks.
func Healthy(st State) bool {
	return len(st.FailedJobs) == 0 && st.Error == "" && (st.Job == nil || st.Job.Phase != Failed || st.Job.CleanFailure())
}
