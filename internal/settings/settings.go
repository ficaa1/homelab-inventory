// Package settings is the configuration a human can change without a redeploy.
//
// Everything here already had an environment variable, and the environment
// variable is still what a fresh install starts from. What this adds is the
// same inversion the catalog Book applies to service names: the env is a
// *seed*, and a value stored by a person is authoritative over it. Changing
// the notification floor should not mean editing /opt/homelab-api/.env over
// SSH and restarting the container, because that is enough friction that the
// setting never gets changed and the notifier quietly stays wrong.
//
// # Unset is not the same as zero
//
// The whole file is pointers, which is the one thing that makes the override
// safe. A stored blob with MinLevel absent must fall through to the
// environment, not to alert.Level("") — otherwise the first write of any
// single field would silently reset every field a human had never touched,
// and deploying with NOTIFY_LEVEL=warning would be overridden by a settings
// file that never mentioned the level at all. This is the same distinction
// the inventory schema draws between a null section and an empty one.
//
// # The apprise URL is write-only
//
// The path segment after /notify/ is the key that decides where a message
// goes, so the URL is a credential. It can be set here, because being able to
// point notifications somewhere new without an SSH session is most of the
// point, and it is never returned — Redacted() reports only enough to tell one
// endpoint from another.
package settings

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

// StateName is the blob in the store.
const StateName = "settings"

// Stored is what a person has explicitly set. Absent means "never set", which
// falls through to the environment — see the package comment.
type Stored struct {
	Auth           AuthStored `json:"auth"`
	AuthGeneration uint64     `json:"auth_generation,omitempty"`
	RunRetention   *Retention `json:"run_retention,omitempty"`
	NotifyLevel    *string    `json:"notify_level,omitempty"`
	NotifyInterval *string    `json:"notify_interval,omitempty"`
	DigestInterval *string    `json:"digest_interval,omitempty"`
	AppriseURL     *string    `json:"apprise_url,omitempty"`
	// DigestAppriseURL is where the weekly summary goes, separate from
	// AppriseURL. A report is a different shape from an alarm: the digest is
	// long-form and only ever read, while transitions are short and urgent.
	// Unset means the digest goes where the transitions go.
	DigestAppriseURL *string `json:"digest_apprise_url,omitempty"`
	// NotifyEnabled turns delivery off without discarding the URL, so muting
	// for an afternoon does not mean re-entering a credential afterwards.
	NotifyEnabled *bool     `json:"notify_enabled,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// Defaults are the values from the environment, used wherever Stored is
// silent.
type Defaults struct {
	Auth             AuthStored
	RunRetention     *store.Policy
	NotifyLevel      alert.Level
	NotifyInterval   time.Duration
	DigestInterval   time.Duration
	AppriseURL       string
	DigestAppriseURL string
}

// Effective is the resolved configuration: what the notifier actually uses.
type Effective struct {
	RunRetention     store.Policy
	NotifyLevel      alert.Level
	NotifyInterval   time.Duration
	DigestInterval   time.Duration
	AppriseURL       string
	DigestAppriseURL string
	NotifyEnabled    bool
}

// Resolve layers Stored over Defaults.
func Resolve(s Stored, d Defaults) Effective {
	e := Effective{
		NotifyLevel:      d.NotifyLevel,
		NotifyInterval:   d.NotifyInterval,
		DigestInterval:   d.DigestInterval,
		AppriseURL:       d.AppriseURL,
		DigestAppriseURL: d.DigestAppriseURL,
		NotifyEnabled:    true,
		RunRetention:     ResolveRetention(s.RunRetention, d.RunRetention),
	}
	if s.NotifyLevel != nil {
		if lv, ok := alert.ParseLevel(*s.NotifyLevel); ok {
			e.NotifyLevel = lv
		}
	}
	if s.NotifyInterval != nil {
		if dur, err := time.ParseDuration(*s.NotifyInterval); err == nil && dur > 0 {
			e.NotifyInterval = dur
		}
	}
	if s.DigestInterval != nil {
		// Zero is meaningful here — it disables the digest — so it is accepted
		// where a zero notify interval is not.
		if dur, err := time.ParseDuration(*s.DigestInterval); err == nil && dur >= 0 {
			e.DigestInterval = dur
		}
	}
	if s.AppriseURL != nil && *s.AppriseURL != "" {
		e.AppriseURL = *s.AppriseURL
	}
	if s.DigestAppriseURL != nil && *s.DigestAppriseURL != "" {
		e.DigestAppriseURL = *s.DigestAppriseURL
	}
	if s.NotifyEnabled != nil {
		e.NotifyEnabled = *s.NotifyEnabled
	}
	return e
}

// Sending reports whether a notification would actually be delivered.
func (e Effective) Sending() bool { return e.NotifyEnabled && e.AppriseURL != "" }

// Update applies a partial change, validating each field before any is
// stored. Validation is all-or-nothing so a typo in the interval cannot leave
// the level changed and the interval not.
type Update struct {
	RunRetention     *Retention `json:"run_retention,omitempty"`
	NotifyLevel      *string    `json:"notify_level,omitempty"`
	NotifyInterval   *string    `json:"notify_interval,omitempty"`
	DigestInterval   *string    `json:"digest_interval,omitempty"`
	AppriseURL       *string    `json:"apprise_url,omitempty"`
	DigestAppriseURL *string    `json:"digest_apprise_url,omitempty"`
	NotifyEnabled    *bool      `json:"notify_enabled,omitempty"`
}

// Apply validates an update and merges it into the stored settings.
func Apply(s Stored, u Update, now time.Time, defaults ...Defaults) (Stored, error) {
	out := s
	if u.RunRetention != nil {
		var next Retention
		if s.RunRetention != nil {
			next = *s.RunRetention
		}
		patch := u.RunRetention
		if patch.KeepAllDays != nil {
			v := *patch.KeepAllDays
			next.KeepAllDays = &v
		}
		if patch.HourlyDays != nil {
			v := *patch.HourlyDays
			next.HourlyDays = &v
		}
		if patch.DailyDays != nil {
			v := *patch.DailyDays
			next.DailyDays = &v
		}
		if patch.CompressAfterDays != nil {
			v := *patch.CompressAfterDays
			next.CompressAfterDays = &v
		}
		var seed *store.Policy
		if len(defaults) > 0 {
			seed = defaults[0].RunRetention
		}
		if err := ResolveRetention(&next, seed).Validate(); err != nil {
			return s, err
		}
		out.RunRetention = &next
	}

	if u.NotifyLevel != nil {
		lv, ok := alert.ParseLevel(strings.TrimSpace(*u.NotifyLevel))
		if !ok {
			return s, fmt.Errorf("notify_level must be one of: critical, warning, info")
		}
		v := string(lv)
		out.NotifyLevel = &v
	}
	if u.NotifyInterval != nil {
		d, err := time.ParseDuration(strings.TrimSpace(*u.NotifyInterval))
		if err != nil || d <= 0 {
			return s, fmt.Errorf(`notify_interval must be a positive duration like "5m"`)
		}
		// The agents push every 15 minutes; evaluating far more often than
		// that is pure load for no faster answer, and evaluating far less
		// often delays every notification by up to that gap.
		if d < time.Minute || d > 6*time.Hour {
			return s, fmt.Errorf("notify_interval must be between 1m and 6h")
		}
		v := d.String()
		out.NotifyInterval = &v
	}
	if u.DigestInterval != nil {
		txt := strings.TrimSpace(*u.DigestInterval)
		d, err := time.ParseDuration(txt)
		if err != nil || d < 0 {
			return s, fmt.Errorf(`digest_interval must be a duration like "168h", or "0" to disable`)
		}
		if d > 0 && d < time.Hour {
			return s, fmt.Errorf("digest_interval must be at least 1h, or 0 to disable")
		}
		v := d.String()
		out.DigestInterval = &v
	}
	if u.AppriseURL != nil {
		txt := strings.TrimSpace(*u.AppriseURL)
		if txt != "" {
			if err := validateAppriseURL(txt); err != nil {
				return s, err
			}
		}
		out.AppriseURL = &txt
	}
	if u.DigestAppriseURL != nil {
		txt := strings.TrimSpace(*u.DigestAppriseURL)
		if txt != "" {
			if err := validateAppriseURL(txt); err != nil {
				return s, fmt.Errorf("digest_%w", err)
			}
		}
		out.DigestAppriseURL = &txt
	}
	if u.NotifyEnabled != nil {
		v := *u.NotifyEnabled
		out.NotifyEnabled = &v
	}

	out.UpdatedAt = now
	return out, nil
}

// validateAppriseURL rejects anything that is not a plain http(s) endpoint.
//
// This value is fetched by the server, so it is the same class of input as an
// icon slug: a scheme like file:// or gopher:// would turn the notifier into a
// way to make the server read local resources.
func validateAppriseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("apprise_url is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("apprise_url must be http:// or https://")
	}
	if u.Host == "" {
		return fmt.Errorf("apprise_url has no host")
	}
	return nil
}

// Redacted describes the apprise endpoint without revealing it.
//
// The key is the path segment after /notify/, so the path is dropped
// entirely; scheme and host are enough to tell "it points at my apprise box"
// from "it points somewhere I did not expect", which is the only question a
// settings page needs to answer about it.
func Redacted(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(set)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// Retention stores only explicitly set ages; zero remains a meaningful value.
type Retention struct {
	KeepAllDays       *int `json:"keepAllDays,omitempty"`
	HourlyDays        *int `json:"hourlyDays,omitempty"`
	DailyDays         *int `json:"dailyDays,omitempty"`
	CompressAfterDays *int `json:"compressAfterDays,omitempty"`
}

func ResolveRetention(s *Retention, seed *store.Policy) store.Policy {
	p := store.DefaultPolicy()
	if seed != nil {
		p = *seed
	}
	if s != nil {
		if s.KeepAllDays != nil {
			p.KeepAllDays = *s.KeepAllDays
		}
		if s.HourlyDays != nil {
			p.HourlyDays = *s.HourlyDays
		}
		if s.DailyDays != nil {
			p.DailyDays = *s.DailyDays
		}
		if s.CompressAfterDays != nil {
			p.CompressAfterDays = *s.CompressAfterDays
		}
	}
	return p
}
