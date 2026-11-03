package agentproto

import "time"

type Update struct {
	Transaction  string    `json:"transaction,omitempty"`
	Active       *Identity `json:"active,omitempty"`
	Fallback     *Identity `json:"fallback,omitempty"`
	Launcher     string    `json:"launcher,omitempty"`
	Sequence     uint64    `json:"sequence"`
	AttemptedAt  time.Time `json:"attempted_at"`
	Result       string    `json:"result"`
	Failures     int       `json:"failures"`
	Mode         string    `json:"mode,omitempty"`
	Updater      string    `json:"updater,omitempty"`
	Target       uint64    `json:"target,omitempty"`
	Stage        string    `json:"stage,omitempty"`
	Code         string    `json:"code,omitempty"`
	FirstFailure *Failure  `json:"firstFailure,omitempty"`
	LastFailure  *Failure  `json:"lastFailure,omitempty"`
	Rejected     uint64    `json:"rejected,omitempty"`
}

// UpdateFailure contains only safe enums, never local error text.
type Failure struct {
	At       time.Time `json:"at"`
	Sequence uint64    `json:"sequence"`
	Stage    string    `json:"stage"`
	Code     string    `json:"code"`
}
