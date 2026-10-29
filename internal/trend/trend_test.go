package trend

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// runs builds a host's history: one inventory per sample, with a ZFS pool
// whose capacity follows pct(i).
func runs(host, pool string, n int, step time.Duration, start time.Time, pct func(i int) int) []*inventory.Inventory {
	var out []*inventory.Inventory
	for i := 0; i < n; i++ {
		p := pct(i)
		out = append(out, &inventory.Inventory{
			SchemaVersion: inventory.SchemaVersion,
			CollectedAt:   start.Add(time.Duration(i) * step),
			Host:          inventory.Host{Hostname: host},
			ZFS: &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{
				Name: pool, Health: "ONLINE", CapacityPct: p,
				SizeBytes:  1 << 40,
				AllocBytes: uint64(float64(uint64(1)<<40) * float64(p) / 100),
			}}},
		})
	}
	return out
}

func series(t *testing.T, r *Report, subject string) Series {
	t.Helper()
	for _, s := range r.Series {
		if s.Subject == subject {
			return s
		}
	}
	t.Fatalf("no series %q in %+v", subject, r.Series)
	return Series{}
}

// The case this package exists to get right, and the state the real fleet is
// in tonight: plenty of readings, not nearly enough time. A slope fitted to
// fourteen hours would extrapolate a single evening's copy into a date.
func TestNoForecastFromTooShortAWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	start := now.Add(-14 * time.Hour)
	// 56 readings over 14 hours, climbing steadily — a naive fit would happily
	// produce a full date.
	invs := runs("host-c", "media", 56, 15*time.Minute, start, func(i int) int { return 76 + i/20 })

	r := Build(invs, now)
	s := series(t, r, "media")

	if s.Forecast != nil {
		t.Errorf("forecast offered from %.1f hours of history: %+v", r.SpanHours, s.Forecast)
	}
	if !strings.Contains(s.Note, "history") {
		t.Errorf("note should explain the shortfall, got %q", s.Note)
	}
	// The refusal must say when it will be worth asking again — "not yet" is
	// only useful with a "yet".
	if !strings.Contains(s.Note, "ask again") {
		t.Errorf("note should say when to come back, got %q", s.Note)
	}
	// The series is still returned: 14 hours makes a fine sparkline.
	if len(s.Points) != 56 || s.Current == 0 {
		t.Errorf("the series itself should still be present, got %d points", len(s.Points))
	}
}

func TestNoForecastFromTooFewReadings(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// A long window but almost no samples: a host that has been posting for a
	// fortnight but only started collecting ZFS recently.
	invs := runs("host-c", "media", 5, 72*time.Hour, now.Add(-15*24*time.Hour),
		func(i int) int { return 70 + i })

	s := series(t, Build(invs, now), "media")
	if s.Forecast != nil {
		t.Errorf("forecast offered from 5 readings: %+v", s.Forecast)
	}
	if !strings.Contains(s.Note, "readings") {
		t.Errorf("note = %q, want it to name the shortfall", s.Note)
	}
}

// Given a long enough window and a clean trend, it should commit to a date.
func TestForecastsWhenTheHistorySupportsIt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// 30 days, 4 readings a day, rising exactly 0.5 points per day from 70.
	// 70 + 0.5*d = 100 at d=60, i.e. 30 days from now.
	start := now.Add(-30 * 24 * time.Hour)
	invs := runs("host-c", "media", 120, 6*time.Hour, start, func(i int) int {
		days := float64(i) * 6 / 24
		return int(70 + 0.5*days)
	})

	s := series(t, Build(invs, now), "media")
	if s.Forecast == nil {
		t.Fatalf("no forecast from 30 days of a clean trend; note = %q", s.Note)
	}
	if math.Abs(s.Forecast.PctPerDay-0.5) > 0.05 {
		t.Errorf("slope = %.3f points/day, want ~0.5", s.Forecast.PctPerDay)
	}
	if s.Forecast.FullAt == nil {
		t.Fatalf("no full date: %+v", s.Forecast)
	}
	if math.Abs(s.Forecast.DaysToFull-30) > 2 {
		t.Errorf("days to full = %.1f, want ~30", s.Forecast.DaysToFull)
	}
	if s.Forecast.Confidence != "reasonable" {
		t.Errorf("confidence = %q on a near-perfect fit (R2=%.2f)", s.Forecast.Confidence, s.Forecast.R2)
	}
}

// Noise must not become a prediction. This is the same failure as forecasting
// from too short a window, arriving by a different route.
func TestNoForecastWhenUsageIsErratic(t *testing.T) {
	t.Parallel()
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour)
	// Sawtooth: fills and is cleared repeatedly, no real trend.
	invs := runs("host-a", "scratch", 120, 6*time.Hour, start, func(i int) int {
		return 40 + (i*7)%40
	})

	s := series(t, Build(invs, now), "scratch")
	if s.Forecast != nil && s.Forecast.FullAt != nil {
		t.Errorf("a sawtooth produced a full date: %+v (R2=%.2f)", s.Forecast, s.Forecast.R2)
	}
	if s.Note == "" {
		t.Error("an erratic series should say why it will not forecast")
	}
}

func TestShrinkingAndFlatAreNotForecastFull(t *testing.T) {
	t.Parallel()
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour)

	shrinking := runs("host-a", "cleaned", 120, 6*time.Hour, start, func(i int) int {
		return 90 - i/4
	})
	s := series(t, Build(shrinking, now), "cleaned")
	if s.Forecast != nil && s.Forecast.FullAt != nil {
		t.Errorf("a shrinking series was forecast to fill: %+v", s.Forecast)
	}
	if s.Forecast == nil || s.Forecast.PctPerDay >= 0 {
		t.Errorf("a shrinking series should report its negative slope, got %+v", s.Forecast)
	}

	flat := runs("host-a", "static", 120, 6*time.Hour, start, func(i int) int { return 55 })
	s = series(t, Build(flat, now), "static")
	if s.Forecast != nil && s.Forecast.FullAt != nil {
		t.Errorf("a flat series was forecast to fill: %+v", s.Forecast)
	}
	// A dead-flat line explains no variance; claiming R2=1 would let it look
	// like the strongest trend on the page.
	if s.Forecast != nil && s.Forecast.R2 > 0 {
		t.Errorf("a flat series reported R2=%.2f", s.Forecast.R2)
	}
}

// A slope so shallow it fills in a decade is arithmetically true and
// practically useless.
func TestVerySlowFillGetsNoDate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	start := now.Add(-60 * 24 * time.Hour)
	// 1 point over 60 days, starting at 10%: ~14 years to full.
	invs := runs("host-c", "archive", 240, 6*time.Hour, start, func(i int) int {
		return 10 + i/240
	})
	s := series(t, Build(invs, now), "archive")
	if s.Forecast != nil && s.Forecast.FullAt != nil {
		t.Errorf("a decade-away fill got a date: %.0f days", s.Forecast.DaysToFull)
	}
}

// ZFS datasets share their pool's free space, so tracking them would plot one
// pool many times over — the trap already documented in portmap and the
// filesystem alert rule.
func TestZFSFilesystemsAreExcluded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   now,
		Host: inventory.Host{Hostname: "host-c", Filesystems: []inventory.Filesystem{
			{MountPoint: "/mnt/media", FSType: "zfs", Kind: inventory.FSKindReal,
				TotalBytes: 1 << 40, UsedBytes: 1 << 39, UsedPct: 50},
			{MountPoint: "/mnt/tank", FSType: "ZFS", Kind: inventory.FSKindReal,
				TotalBytes: 1 << 40, UsedBytes: 1 << 39, UsedPct: 50},
			{MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
				TotalBytes: 1 << 40, UsedBytes: 1 << 39, UsedPct: 50},
			{MountPoint: "/snap/x", FSType: "squashfs", Kind: inventory.FSKindSnap,
				TotalBytes: 1 << 20, UsedBytes: 1 << 20, UsedPct: 100},
		}},
	}
	r := Build([]*inventory.Inventory{inv}, now)
	for _, s := range r.Series {
		if strings.HasPrefix(s.Subject, "/mnt/") {
			t.Errorf("a ZFS filesystem was tracked: %q", s.Subject)
		}
		if strings.HasPrefix(s.Subject, "/snap/") {
			t.Errorf("a snap mount was tracked: %q", s.Subject)
		}
	}
	if len(r.Series) != 1 || r.Series[0].Subject != "/" {
		t.Errorf("series = %+v, want just /", r.Series)
	}
}

func TestSeriesReportsMovementAndOrdersByFullest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	mk := func(pool string, pct int) inventory.ZPool {
		return inventory.ZPool{Name: pool, CapacityPct: pct, SizeBytes: 1000,
			AllocBytes: uint64(10 * pct)}
	}
	older := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, CollectedAt: now.Add(-24 * time.Hour),
		Host: inventory.Host{Hostname: "host-c"},
		ZFS:  &inventory.ZFS{Available: true, Pools: []inventory.ZPool{mk("media", 70), mk("secure", 50)}},
	}
	newer := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, CollectedAt: now,
		Host: inventory.Host{Hostname: "host-c"},
		ZFS:  &inventory.ZFS{Available: true, Pools: []inventory.ZPool{mk("media", 76), mk("secure", 55)}},
	}
	// Deliberately out of order: Build must sort by time itself.
	r := Build([]*inventory.Inventory{newer, older}, now)

	if len(r.Series) != 2 || r.Series[0].Subject != "media" {
		t.Fatalf("series = %+v, want media first (it is fullest)", r.Series)
	}
	m := series(t, r, "media")
	if m.Current != 76 {
		t.Errorf("Current = %v, want 76 (the newest reading)", m.Current)
	}
	if m.ChangePct != 6 {
		t.Errorf("ChangePct = %v, want 6", m.ChangePct)
	}
	if m.ChangeBytes != 60 {
		t.Errorf("ChangeBytes = %v, want 60", m.ChangeBytes)
	}
	if r.Runs != 2 || math.Abs(r.SpanHours-24) > 0.01 {
		t.Errorf("Runs=%d SpanHours=%.2f, want 2 and 24", r.Runs, r.SpanHours)
	}
}

// A run where a collector did not fire is a gap in a line, not a zero.
func TestMissingSectionsAreGapsNotZeroes(t *testing.T) {
	t.Parallel()
	now := time.Now()
	withZFS := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, CollectedAt: now.Add(-time.Hour),
		Host: inventory.Host{Hostname: "host-c"},
		ZFS: &inventory.ZFS{Available: true, Pools: []inventory.ZPool{
			{Name: "media", CapacityPct: 76, SizeBytes: 100, AllocBytes: 76}}},
	}
	withoutZFS := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion, CollectedAt: now,
		Host: inventory.Host{Hostname: "host-c"},
		ZFS:  nil, // the collector did not run
	}
	r := Build([]*inventory.Inventory{withZFS, withoutZFS}, now)
	s := series(t, r, "media")
	if len(s.Points) != 1 {
		t.Fatalf("got %d points, want 1 — a missing section must not become a reading", len(s.Points))
	}
	if s.Points[0].Pct != 76 {
		t.Errorf("point = %v, want the real reading", s.Points[0].Pct)
	}
}

func TestEmptyHistory(t *testing.T) {
	t.Parallel()
	r := Build(nil, time.Now())
	if r == nil || len(r.Series) != 0 {
		t.Errorf("empty history should give an empty report, got %+v", r)
	}
}
