package webui

import (
	"regexp"
	"testing"
)

// A delayed 401 from a superseded refresh must not discard the current login.
func TestOverviewRejectsSupersededErrorsBeforeAuth(t *testing.T) {
	page := body(t)
	guard := regexp.MustCompile(`(?s)function loadOverview\(\).*?function \(err\) \{\s*if \(seq !== overviewSeq\) return;\s*if \(err.unauthorized\)`)
	if !guard.MatchString(page) {
		t.Fatal("late overview authorization errors can log out a newer load")
	}
}
