package fleet

import "time"

type policy struct {
	StaleSeconds int64   `json:"stale_after_seconds"`
	WarnFree     float64 `json:"disk_warn_free_ratio"`
	CritFree     float64 `json:"disk_crit_free_ratio"`
	DueSoon      int     `json:"trial_due_soon_days"`
}

type hostCounts struct {
	Drift, Refused, Pending, Updates int
	Broken                           int `json:"broken_links"`
}
type disk struct {
	Total int64 `json:"total_bytes"`
	Free  int64 `json:"free_bytes"`
}
type contextCost struct {
	Harness string
	Skills  int `json:"model_invoked"`
	Chars   int `json:"description_chars"`
	Tokens  int `json:"tokens_estimate"`
}
type hostSummary struct {
	Name    string
	Version *string    `json:"fleet_version"`
	Seen    *time.Time `json:"last_seen_at"`
	Commit  *string    `json:"synced_commit"`
	Counts  *hostCounts
	Disk    *disk
	Cost    []contextCost `json:"context_cost"`
}
type hostEnvelope struct {
	envelope
	Head   string `json:"repo_head"`
	Policy *policy
}
type hostsResponse struct {
	hostEnvelope
	Hosts []hostSummary
}
type trialDates struct{ Since, Ends, State string }
type overviewResponse struct {
	hostsResponse
	Trials []struct {
		trialDates
		Source string
	}
	Recent []struct {
		Action, State, Summary string
		Created                time.Time `json:"created_at"`
		Pending                int       `json:"pending_hosts"`
	}
}
type hostResponse struct {
	hostEnvelope
	Host   hostSummary
	Report *report
}
type report struct {
	OK    *int `json:"ok_count"`
	Items []struct {
		Path, Harness, Kind, State, Detail string
		Diff                               *struct {
			Added   int `json:"lines_added"`
			Removed int `json:"lines_removed"`
		}
	}
	Providers []struct{ Harness, Provider, Endpoint string }
	Updates   []struct{ Component, Installed, Latest, Apply string }
	Requests  []struct {
		Item, State string
		Requested   time.Time `json:"requested_at"`
	} `json:"force_requests"`
}

type source struct {
	Name, Origin string
	Defined      string `json:"defined_in"`
	Trial        *trialDates
}
type hostUsage struct {
	Host    string
	Threads int
}
type installation struct {
	Host      string
	Installed bool
}
type skill struct {
	Name, Mode string
	Source     *string
	ModeFrom   string `json:"mode_from"`
	DecidedAt  string `json:"decided_at"`
	Hosts      []installation
	Usage      *struct {
		Threads int
		ByHost  []hostUsage `json:"by_host"`
	}
}
type skillsResponse struct {
	envelope
	Window    int `json:"usage_window_days"`
	Sources   []source
	Skills    []skill
	Truncated bool
}
type trial struct {
	trialDates
	Source string
	Hosts  []string
	Skills []struct {
		Name, Mode, Suggestion string
		Decided                bool
		Threads                *int
		ByHost                 []hostUsage `json:"by_host"`
	}
}
type trialsResponse struct {
	envelope
	Trials []trial
}
