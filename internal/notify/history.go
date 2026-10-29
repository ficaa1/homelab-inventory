package notify

import (
	"sync"
	"time"
)

// LogName is the delivery log's key in the store.
const LogName = "notify-log"

// maxLog is how many deliveries are kept. Enough to answer "did this ever
// work, and when", and small enough that the blob stays trivial to write on
// every send.
const maxLog = 50

// Delivery is one attempt to send one message.
//
// This exists because of a question the status endpoint could not answer.
// "Notifications are on, floor is critical, nothing sent" is indistinguishable
// to a reader from "notifications are broken" — both look like silence, and
// the design deliberately produces a lot of silence. A log of attempts, with
// the failures kept rather than only the successes, is what separates working
// and quiet from broken and quiet.
type Delivery struct {
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	// Type is apprise's notification type: info, success, warning, failure.
	Type string `json:"type"`
	// Kind distinguishes a transition from a digest from a manual test, so a
	// page can say "the only thing that ever arrived was a test" — which is
	// exactly the situation this was built to make visible.
	Kind string `json:"kind"`
	OK   bool   `json:"ok"`
	// Error is the failure reason, already stripped of the apprise URL by the
	// sender. Never populated on success.
	Error string `json:"error,omitempty"`
}

// Log is the recent delivery history.
type Log struct {
	Deliveries []Delivery `json:"deliveries"`
	// Attempted and Delivered count every attempt ever, not just the ones
	// still in the window, so a page can distinguish "never tried" from
	// "tried a hundred times and the last fifty are shown".
	Attempted int `json:"attempted"`
	Delivered int `json:"delivered"`
}

// Recorder appends deliveries to the store.
//
// Failures are recorded as well as successes, which is the opposite of what
// the notifier does with its state: state is advanced only on success so an
// outage retries, while the log wants the outage in it precisely because that
// is the evidence a reader is looking for.
type Recorder struct {
	State StateStore

	mu sync.Mutex
}

// Record appends one delivery, trimming to the newest maxLog.
func (r *Recorder) Record(d Delivery) {
	if r == nil || r.State == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var log Log
	// A missing or unreadable log must not stop a notification being sent, so
	// the error is dropped and the log restarts rather than propagating.
	_ = r.State.ReadState(LogName, &log)

	log.Attempted++
	if d.OK {
		log.Delivered++
	}
	log.Deliveries = append([]Delivery{d}, log.Deliveries...)
	if len(log.Deliveries) > maxLog {
		log.Deliveries = log.Deliveries[:maxLog]
	}
	_ = r.State.WriteState(LogName, log)
}

// Read returns the log, empty if there is none.
func (r *Recorder) Read() Log {
	var log Log
	if r == nil || r.State == nil {
		return log
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.State.ReadState(LogName, &log)
	if log.Deliveries == nil {
		log.Deliveries = []Delivery{}
	}
	return log
}
