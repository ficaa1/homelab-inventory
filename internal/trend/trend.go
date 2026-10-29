// Package trend turns the stored run history into capacity over time, and —
// when the data actually supports it — a forecast of when something fills.
//
// # Why this refuses to answer more often than it answers
//
// A least-squares fit will happily return a slope for any two points, and a
// slope trivially converts into "media fills in 3 days". That number would be
// indistinguishable, in the UI, from one backed by a month of data. It would
// also be wrong most of the time: a pool that gains 40 GB because somebody
// copied a season of something, sampled over an afternoon, extrapolates to
// full-by-Thursday.
//
// So the forecast is gated on the history being able to support it — a minimum
// span, a minimum number of points, a positive slope, and a fit that actually
// explains the variance — and when a gate fails the reason is returned instead
// of a number. "Not enough history yet, ask again in four days" is a useful
// answer. A confident wrong date is not.
//
// The series itself is always returned, because fourteen hours of data makes a
// perfectly good sparkline even though it makes a terrible prediction.
//
// # Which sources
//
// ZFS pools, Proxmox storages and real filesystems. Deliberately *not* ZFS
// datasets: they share their pool's free space, so a per-dataset series counts
// one pool many times over — the same trap documented in the port map and in
// the filesystem alert rule.
package trend

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Kind says where a series came from, so a UI can group or filter.
type Kind string

const (
	KindZFSPool    Kind = "zfs-pool"
	KindPVEStorage Kind = "pve-storage"
	KindFilesystem Kind = "filesystem"
)

// Point is one observation.
type Point struct {
	At  time.Time `json:"at"`
	Pct float64   `json:"pct"`
	// UsedBytes is carried so a UI can say "+40 GiB this week" rather than
	// "+2 points", which is the more useful sentence for a big pool.
	UsedBytes  uint64 `json:"used_bytes,omitempty"`
	TotalBytes uint64 `json:"total_bytes,omitempty"`
}

// Series is one thing whose capacity was tracked over time.
type Series struct {
	Subject string `json:"subject"`
	Kind    Kind   `json:"kind"`
	// Type is the storage type for a PVE storage ("lvmthin", "dir", ...),
	// which is what decides how alarming a given percentage is.
	Type    string  `json:"type,omitempty"`
	Points  []Point `json:"points"`
	Current float64 `json:"current"`
	// ChangeBytes is the movement across the whole window, which is the plain
	// fact underneath any fit.
	ChangeBytes int64   `json:"change_bytes"`
	ChangePct   float64 `json:"change_pct"`

	// Forecast is nil whenever the history cannot support one. Note always
	// explains the situation, whether or not a forecast came with it.
	Forecast *Forecast `json:"forecast,omitempty"`
	Note     string    `json:"note,omitempty"`
}

// Forecast is a straight line through the observations, and the date it
// crosses full.
type Forecast struct {
	// PctPerDay is the fitted slope. Negative means it is emptying, in which
	// case there is no full date.
	PctPerDay float64 `json:"pct_per_day"`
	// R2 is how much of the variance the line explains, 0 to 1. It is exposed
	// rather than hidden because it is the number that says whether to believe
	// the date.
	R2 float64 `json:"r2"`
	// FullAt is when the fit reaches 100%. Nil when it never does within a
	// useful horizon.
	FullAt     *time.Time `json:"full_at,omitempty"`
	DaysToFull float64    `json:"days_to_full,omitempty"`
	// Confidence is "low" or "reasonable", derived from R2. There is
	// deliberately no "high": a straight line through a fortnight of a homelab
	// disk does not earn that word.
	Confidence string `json:"confidence"`
}

// Report is one host's capacity history.
type Report struct {
	Host string    `json:"host"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Runs is how many stored runs the series were built from, and SpanHours
	// how long they cover. Both are reported because they are what determines
	// whether any forecast was possible.
	Runs      int      `json:"runs"`
	SpanHours float64  `json:"span_hours"`
	Series    []Series `json:"series"`
}

// The gates. A forecast is only offered when every one of these holds.
const (
	// minPoints: fewer than this and a slope is an artefact of two readings.
	minPoints = 12
	// minSpan: below three days, a single large copy dominates the fit
	// completely. The agents post every 15 minutes, so this is about the
	// window being wide enough to contain ordinary daily variation, not about
	// collecting enough samples.
	minSpan = 72 * time.Hour
	// minR2: below this the points are scatter, not a trend, and the line is
	// describing noise.
	minR2 = 0.30
	// goodR2: above this the line explains most of the movement.
	goodR2 = 0.75
	// maxHorizon: a slope so shallow that it fills in over ten years is
	// arithmetically true and practically meaningless.
	maxHorizon = 3650.0
)

// Sample is one observation of one series, before any grouping. A stored run
// yields its samples once, so a caller can keep them and assemble a report later.
type Sample struct {
	Kind    Kind
	Subject string
	Type    string
	Point   Point
}

// Samples extracts the observations one run contributes. A run without a
// collection time contributes none.
func Samples(inv *inventory.Inventory) []Sample {
	if inv == nil || inv.CollectedAt.IsZero() {
		return nil
	}
	at := inv.CollectedAt
	var out []Sample
	if inv.ZFS != nil && inv.ZFS.Available {
		for _, p := range inv.ZFS.Pools {
			if p.SizeBytes == 0 {
				continue
			}
			out = append(out, Sample{Kind: KindZFSPool, Subject: p.Name, Point: Point{
				At: at, Pct: float64(p.CapacityPct),
				UsedBytes: p.AllocBytes, TotalBytes: p.SizeBytes,
			}})
		}
	}
	if inv.Proxmox != nil && inv.Proxmox.Available {
		for _, st := range inv.Proxmox.Storages {
			if st.TotalBytes == 0 {
				continue
			}
			out = append(out, Sample{Kind: KindPVEStorage, Subject: st.Name, Type: st.Type, Point: Point{
				At:        at,
				Pct:       100 * float64(st.UsedBytes) / float64(st.TotalBytes),
				UsedBytes: st.UsedBytes, TotalBytes: st.TotalBytes,
			}})
		}
	}
	for _, fs := range inv.Host.Filesystems {
		// Same exclusion as the filesystem alert rule: a ZFS dataset's
		// statfs reports its whole pool's free space, so tracking datasets
		// would plot one pool many times over.
		if fs.Kind != inventory.FSKindReal || fs.TotalBytes == 0 ||
			equalFold(fs.FSType, "zfs") {
			continue
		}
		out = append(out, Sample{Kind: KindFilesystem, Subject: fs.MountPoint, Point: Point{
			At: at, Pct: fs.UsedPct,
			UsedBytes: fs.UsedBytes, TotalBytes: fs.TotalBytes,
		}})
	}
	return out
}

// Assemble groups samples into series. Samples are taken in the order given:
// when a storage's type changes, the last sample's type is the one reported.
func Assemble(samples []Sample, now time.Time) *Report {
	rep := &Report{Series: []Series{}}
	type key struct {
		kind    Kind
		subject string
	}
	points := map[key][]Point{}
	types := map[key]string{}
	for _, s := range samples {
		k := key{s.Kind, s.Subject}
		if s.Kind == KindPVEStorage {
			types[k] = s.Type
		}
		points[k] = append(points[k], s.Point)
	}

	for k, pts := range points {
		sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
		s := Series{Subject: k.subject, Kind: k.kind, Type: types[k], Points: pts}
		s.Current = pts[len(pts)-1].Pct
		s.ChangePct = pts[len(pts)-1].Pct - pts[0].Pct
		s.ChangeBytes = int64(pts[len(pts)-1].UsedBytes) - int64(pts[0].UsedBytes)
		s.Forecast, s.Note = forecast(pts, now)
		rep.Series = append(rep.Series, s)

		if rep.From.IsZero() || pts[0].At.Before(rep.From) {
			rep.From = pts[0].At
		}
		if last := pts[len(pts)-1].At; last.After(rep.To) {
			rep.To = last
		}
	}

	sort.Slice(rep.Series, func(i, j int) bool {
		// Fullest first: it is the one that matters.
		if rep.Series[i].Current != rep.Series[j].Current {
			return rep.Series[i].Current > rep.Series[j].Current
		}
		return rep.Series[i].Subject < rep.Series[j].Subject
	})
	if !rep.From.IsZero() {
		rep.SpanHours = rep.To.Sub(rep.From).Hours()
	}
	return rep
}

// Build assembles the capacity history for one host from its stored runs.
//
// invs may arrive in any order and may contain runs where a section was not
// collected; both are normal. A subject that appears in only some runs simply
// has fewer points.
func Build(invs []*inventory.Inventory, now time.Time) *Report {
	runs := make([]Run, len(invs))
	for i, inv := range invs {
		runs[i] = Extract(inv)
	}
	return BuildRuns(runs, now)
}

// forecast fits a line and decides whether to believe it.
//
// Returning a note instead of a number is the normal outcome early on, and is
// the point: an honest "not yet" beats a confident date derived from an
// afternoon of readings.
func forecast(pts []Point, now time.Time) (*Forecast, string) {
	if len(pts) < minPoints {
		return nil, fmt.Sprintf("only %d readings so far; a forecast needs at least %d",
			len(pts), minPoints)
	}
	span := pts[len(pts)-1].At.Sub(pts[0].At)
	if span < minSpan {
		short := minSpan - span
		return nil, fmt.Sprintf(
			"only %s of history; a forecast needs %s, so ask again in about %s",
			roughSpan(span), roughSpan(minSpan), roughSpan(short))
	}

	slope, intercept, r2 := fitLine(pts)

	if slope <= 0 {
		return &Forecast{PctPerDay: slope, R2: r2, Confidence: confidence(r2)},
			"not filling — usage is flat or falling over this window"
	}
	if r2 < minR2 {
		return nil, fmt.Sprintf(
			"usage moves too erratically to extrapolate (the trend explains only %.0f%% of it)",
			r2*100)
	}

	// Days from the first observation until the line reaches 100.
	daysFromStart := (100 - intercept) / slope
	full := pts[0].At.Add(time.Duration(daysFromStart * 24 * float64(time.Hour)))
	daysFromNow := full.Sub(now).Hours() / 24

	f := &Forecast{PctPerDay: slope, R2: r2, Confidence: confidence(r2)}
	if daysFromNow > maxHorizon {
		return f, "filling, but so slowly that it is not worth a date"
	}
	if daysFromNow < 0 {
		// The fit says it should already be full. That means the recent points
		// bent away from the line, so the line is not describing the present.
		return f, "the trend has broken down — the fit is already past full"
	}
	f.FullAt = &full
	f.DaysToFull = daysFromNow
	return f, ""
}

func confidence(r2 float64) string {
	if r2 >= goodR2 {
		return "reasonable"
	}
	return "low"
}

// fitLine is ordinary least squares of percentage against days, returning the
// slope in points per day, the intercept at the first observation, and R².
func fitLine(pts []Point) (slope, intercept, r2 float64) {
	n := float64(len(pts))
	t0 := pts[0].At

	var sumX, sumY, sumXY, sumXX float64
	for _, p := range pts {
		x := p.At.Sub(t0).Hours() / 24
		sumX += x
		sumY += p.Pct
		sumXY += x * p.Pct
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0, sumY / n, 0
	}
	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssTot, ssRes float64
	for _, p := range pts {
		x := p.At.Sub(t0).Hours() / 24
		pred := intercept + slope*x
		ssRes += (p.Pct - pred) * (p.Pct - pred)
		ssTot += (p.Pct - meanY) * (p.Pct - meanY)
	}
	if ssTot == 0 {
		// Perfectly flat. The line is exact, but calling that R²=1 would let a
		// dead-flat series claim a strong trend.
		return slope, intercept, 0
	}
	r2 = 1 - ssRes/ssTot
	if math.IsNaN(r2) || r2 < 0 {
		r2 = 0
	}
	return slope, intercept, r2
}

func roughSpan(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
