package agentinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentmount"
)

const cronDescription = "homelab-inventory-agent"

// CheckDataset is also available in agentmount for the runtime launcher, without
// making the launcher import its installer.
func CheckDataset(dir string) error { return agentmount.CheckDataset(dir) }

// CronSchedule uses the field names accepted by the TrueNAS middleware API.
type CronSchedule struct {
	Minute string `json:"minute"`
	Hour   string `json:"hour"`
	DOM    string `json:"dom"`
	Month  string `json:"month"`
	DOW    string `json:"dow"`
}

// CronJob is a persistable snapshot. Raw retains additional query fields so a
// predecessor's complete configuration can be compared before any mutation.
type CronJob struct {
	ID          int64           `json:"id"`
	User        string          `json:"user"`
	Command     string          `json:"command"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	Stdout      bool            `json:"stdout"`
	Stderr      bool            `json:"stderr"`
	Schedule    CronSchedule    `json:"schedule"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

// CronSnapshot must be saved before Commit, then again after Commit (including
// errors). A pre-Commit snapshot is sufficient for crash recovery via Rollback.
// It contains cron configuration, never predecessor files or credentials.
type CronSnapshot struct {
	Dir         string   `json:"dir"`
	Prior       *CronJob `json:"prior,omitempty"`
	Predecessor *CronJob `json:"predecessor,omitempty"`
	CreatedID   int64    `json:"created_id,omitempty"`
	Created     *CronJob `json:"created,omitempty"`
}

// CronPlan is prepared without mutation. Commit activates cron only after the
// installer has verified that the replacement agent's report was accepted.
type CronPlan struct {
	snapshot CronSnapshot
}

func (p *CronPlan) Snapshot() CronSnapshot {
	// Do not let a caller mutate the live rollback state through pointer fields.
	b, _ := json.Marshal(p.snapshot)
	var s CronSnapshot
	_ = json.Unmarshal(b, &s)
	return s
}

// RestoreCronPlan reconstructs a rollback plan saved by the installer.
func RestoreCronPlan(s CronSnapshot) (*CronPlan, error) {
	if !validCronDir(s.Dir) || s.CreatedID < 0 || s.Prior != nil && s.CreatedID != 0 || s.Created != nil && (s.CreatedID == 0 || s.Created.ID != s.CreatedID) {
		return nil, errors.New("invalid saved cron plan")
	}
	for _, j := range []*CronJob{s.Prior, s.Predecessor, s.Created} {
		if j == nil {
			continue
		}
		parsed, err := parseCronJob(j.Raw)
		if err != nil || !reflect.DeepEqual(parsed, *j) {
			return nil, errors.New("invalid saved cron job")
		}
	}
	want := desiredCron(s.Dir)
	if s.Prior != nil && !ownCron(*s.Prior, want) || s.Created != nil && !sameConfig(*s.Created, want) || s.Predecessor != nil && (cronIdentity(*s.Predecessor, want) || s.Prior != nil && s.Prior.ID == s.Predecessor.ID) {
		return nil, errors.New("invalid saved cron identity")
	}
	p := &CronPlan{snapshot: s}
	p.snapshot = p.Snapshot()
	return p, nil
}

// PrepareCron queries only. replace is empty, a positive decimal ID, or an exact
// description. Numeric strings always mean IDs; ambiguous selections fail closed.
func PrepareCron(ctx context.Context, dir, replace string) (*CronPlan, error) {
	if !validCronDir(dir) {
		return nil, errors.New("invalid cron state directory")
	}
	if err := CheckDataset(dir); err != nil {
		return nil, err
	}
	jobs, err := queryCron(ctx)
	if err != nil {
		return nil, err
	}
	want := desiredCron(dir)
	own, err := findOwnCron(jobs, want)
	if err != nil {
		return nil, err
	}
	p := &CronPlan{snapshot: CronSnapshot{Dir: dir, Prior: own}}
	if replace == "" {
		return p, nil
	}
	var selected *CronJob
	numeric := true
	for _, ch := range replace {
		if ch < '0' || ch > '9' {
			numeric = false
		}
	}
	id, idErr := strconv.ParseInt(replace, 10, 64)
	if numeric && (idErr != nil || id <= 0) {
		return nil, errors.New("invalid predecessor cron ID")
	}
	for _, job := range jobs {
		if numeric && job.ID == id || !numeric && job.Description == replace {
			if selected != nil {
				return nil, errors.New("predecessor cron selection is ambiguous")
			}
			copy := job
			selected = &copy
		}
	}
	if selected == nil || own != nil && selected.ID == own.ID || cronIdentity(*selected, want) {
		return nil, errors.New("predecessor cron selection is missing or belongs to this agent")
	}
	if strings.Contains(selected.Command, dir+"/") {
		return nil, errors.New("choose a new state directory separate from predecessor files")
	}
	p.snapshot.Predecessor = selected
	return p, nil
}

// Commit creates or updates the one stable root job, then disables an explicitly
// selected predecessor after re-querying its complete original configuration.
// Call Rollback on errors; even an unsuccessful midclt call may have taken effect.
func (p *CronPlan) Commit(ctx context.Context) error {
	if err := CheckDataset(p.snapshot.Dir); err != nil {
		return err
	}
	jobs, err := queryCron(ctx)
	if err != nil {
		return err
	}
	want := desiredCron(p.snapshot.Dir)
	current, err := findOwnCron(jobs, want)
	if err != nil {
		return err
	}
	if err := p.checkPredecessor(jobs, false); err != nil {
		return err
	}
	prior := p.snapshot.Prior
	if prior != nil {
		if current == nil || current.ID != prior.ID || !sameRaw(*current, *prior) {
			return errors.New("own cron changed since preparation")
		}
		if !sameConfig(*current, want) {
			if _, err := mutateCron(ctx, "update", current.ID, cronConfig(want)); err != nil {
				return err
			}
		}
	} else {
		if current != nil {
			return errors.New("own cron appeared since preparation")
		}
		b, err := mutateCron(ctx, "create", 0, cronConfig(want))
		if err != nil {
			return err
		}
		// TrueNAS returns the created job object. Validate it before trusting its ID.
		created, err := parseCronJob(b)
		if err != nil || !sameConfig(created, want) {
			return errors.New("invalid cron create response; rollback required")
		}
		p.snapshot.CreatedID, p.snapshot.Created = created.ID, &created
	}
	jobs, err = queryCron(ctx)
	if err != nil {
		return err
	}
	current, err = findOwnCron(jobs, want)
	if err != nil {
		return err
	}
	expectedID := p.snapshot.CreatedID
	if prior != nil {
		expectedID = prior.ID
	}
	if current == nil || current.ID != expectedID || !sameConfig(*current, want) {
		return errors.New("own cron activation could not be verified")
	}
	if err := p.checkPredecessor(jobs, false); err != nil {
		return err
	}
	if old := p.snapshot.Predecessor; old != nil && old.Enabled {
		if _, err := mutateCron(ctx, "update", old.ID, map[string]any{"enabled": false}); err != nil {
			return err
		}
		jobs, err = queryCron(ctx)
		if err != nil {
			return err
		}
		if err := p.checkPredecessor(jobs, true); err != nil {
			return err
		}
		current, err = findOwnCron(jobs, want)
		if err != nil {
			return err
		}
		if current == nil || current.ID != expectedID || !sameConfig(*current, want) {
			return errors.New("own cron changed during predecessor activation")
		}
	}
	return nil
}

// Rollback refuses to overwrite concurrent edits. Only the selected predecessor's
// enabled flag is restored; its command and files are never changed.
func (p *CronPlan) Rollback(ctx context.Context) error {
	jobs, err := queryCron(ctx)
	if err != nil {
		return err
	}
	want := desiredCron(p.snapshot.Dir)
	if err := p.checkPredecessorRollback(jobs); err != nil {
		return err
	}
	if old := p.snapshot.Predecessor; old != nil && old.Enabled {
		now := cronByID(jobs, old.ID)
		if !now.Enabled {
			if _, err := mutateCron(ctx, "update", old.ID, map[string]any{"enabled": true}); err != nil {
				return err
			}
		}
	}
	// Re-query before own-job mutation as restoring the predecessor may take time.
	jobs, err = queryCron(ctx)
	if err != nil {
		return err
	}
	// Restore and verify the old reporting path even if someone independently
	// edited the own job. Do not remove that job until the predecessor is safe.
	if err := p.checkPredecessor(jobs, false); err != nil {
		return err
	}
	current, err := findOwnCron(jobs, want)
	if err != nil {
		return err
	}
	if err := p.checkRollbackOwn(current, want); err != nil {
		return err
	}
	if old := p.snapshot.Prior; old != nil {
		if !sameRaw(*current, *old) {
			_, err = mutateCron(ctx, "update", old.ID, cronConfig(*old))
		}
	} else if current != nil {
		_, err = mutateCron(ctx, "delete", current.ID, nil)
	}
	if err != nil {
		return err
	}
	jobs, err = queryCron(ctx)
	if err != nil {
		return err
	}
	current, err = findOwnCron(jobs, want)
	if err != nil {
		return err
	}
	if p.snapshot.Prior == nil && current != nil || p.snapshot.Prior != nil && (current == nil || !sameRaw(*current, *p.snapshot.Prior)) {
		return errors.New("own cron rollback could not be verified")
	}
	return p.checkPredecessor(jobs, false)
}

func (p *CronPlan) checkRollbackOwn(current *CronJob, want CronJob) error {
	if old := p.snapshot.Prior; old != nil {
		if current == nil || current.ID != old.ID || !sameRaw(*current, *old) && !sameRawConfig(*current, *old, want) {
			return errors.New("own cron changed; rollback refused")
		}
	} else if current != nil {
		if !sameConfig(*current, want) || p.snapshot.CreatedID != 0 && current.ID != p.snapshot.CreatedID || p.snapshot.Created != nil && !sameRaw(*current, *p.snapshot.Created) {
			return errors.New("created cron changed; rollback refused")
		}
	}
	return nil
}

func (p *CronPlan) checkPredecessor(jobs []CronJob, disabled bool) error {
	old := p.snapshot.Predecessor
	if old == nil {
		return nil
	}
	now := cronByID(jobs, old.ID)
	want := *old
	if disabled {
		want.Enabled = false
	}
	if now == nil || !sameRawConfig(*now, *old, want) {
		return errors.New("predecessor cron changed since preparation")
	}
	return nil
}

func (p *CronPlan) checkPredecessorRollback(jobs []CronJob) error {
	if p.checkPredecessor(jobs, false) == nil || p.checkPredecessor(jobs, true) == nil {
		return nil
	}
	return errors.New("predecessor cron changed; rollback refused")
}

func validCronDir(dir string) bool {
	return filepath.IsAbs(dir) && filepath.Clean(dir) == dir && !strings.ContainsAny(dir, "\x00\r\n%")
}

func desiredCron(dir string) CronJob {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	return CronJob{User: "root", Description: cronDescription, Command: quote(filepath.Join(dir, "agentboot")) + " -config " + quote(filepath.Join(dir, "config.json")), Enabled: true, Schedule: CronSchedule{Minute: "*/15", Hour: "*", DOM: "*", Month: "*", DOW: "*"}}
}

func cronIdentity(job, want CronJob) bool {
	return job.Description == want.Description || sameCommand(job.Command, want.Command)
}

func ownCron(job, want CronJob) bool {
	return job.User == "root" && job.Description == want.Description && sameCommand(job.Command, want.Command)
}

func findOwnCron(jobs []CronJob, want CronJob) (*CronJob, error) {
	var own *CronJob
	for _, job := range jobs {
		if !cronIdentity(job, want) {
			continue
		}
		if own != nil {
			return nil, errors.New("duplicate agent cron identity or command")
		}
		if !ownCron(job, want) {
			return nil, errors.New("agent cron identity collides with another job")
		}
		copy := job
		own = &copy
	}
	return own, nil
}

func cronByID(jobs []CronJob, id int64) *CronJob {
	for i := range jobs {
		if jobs[i].ID == id {
			return &jobs[i]
		}
	}
	return nil
}

func cronConfig(j CronJob) map[string]any {
	return map[string]any{"user": j.User, "command": j.Command, "description": j.Description, "enabled": j.Enabled, "stdout": j.Stdout, "stderr": j.Stderr, "schedule": j.Schedule}
}

func sameConfig(a, b CronJob) bool {
	return reflect.DeepEqual(cronConfig(a), cronConfig(b))
}

func sameRaw(a, b CronJob) bool { return sameRawConfig(a, b, b) }

func sameRawConfig(now, original, config CronJob) bool {
	a, errA := strictJSON(now.Raw)
	b, errB := strictJSON(original.Raw)
	am, okA := a.(map[string]any)
	bm, okB := b.(map[string]any)
	if errA != nil || errB != nil || !okA || !okB || now.ID != original.ID {
		return false
	}
	configJSON, _ := json.Marshal(cronConfig(config))
	c, _ := strictJSON(configJSON)
	for key, value := range c.(map[string]any) {
		bm[key] = value
	}
	return reflect.DeepEqual(am, bm)
}

func queryCron(ctx context.Context) ([]CronJob, error) {
	b, err := callMidclt(ctx, "query")
	if err != nil {
		return nil, err
	}
	value, err := strictJSON(b)
	if err != nil {
		return nil, errors.New("invalid cron query response")
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("invalid cron query response")
	}
	jobs := make([]CronJob, 0, len(items))
	ids := make(map[int64]bool)
	for _, item := range items {
		b, _ := json.Marshal(item)
		job, err := parseCronJob(b)
		if err != nil || ids[job.ID] {
			return nil, errors.New("invalid or duplicate cron query identity")
		}
		ids[job.ID] = true
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func parseCronJob(b []byte) (CronJob, error) {
	invalid := errors.New("invalid cron job response")
	value, err := strictJSON(b)
	m, ok := value.(map[string]any)
	if err != nil || !ok {
		return CronJob{}, invalid
	}
	var job CronJob
	id, ok := m["id"].(json.Number)
	if !ok {
		return job, invalid
	}
	job.ID, err = strconv.ParseInt(string(id), 10, 64)
	if err != nil || job.ID <= 0 {
		return job, invalid
	}
	for key, target := range map[string]*string{"user": &job.User, "command": &job.Command, "description": &job.Description} {
		v, ok := m[key].(string)
		if !ok || strings.ContainsRune(v, 0) {
			return job, invalid
		}
		*target = v
	}
	if job.User == "" || job.Command == "" {
		return job, invalid
	}
	for key, target := range map[string]*bool{"enabled": &job.Enabled, "stdout": &job.Stdout, "stderr": &job.Stderr} {
		v, ok := m[key].(bool)
		if !ok {
			return job, invalid
		}
		*target = v
	}
	schedule, ok := m["schedule"].(map[string]any)
	if !ok || len(schedule) != 5 {
		return job, invalid
	}
	for key, target := range map[string]*string{"minute": &job.Schedule.Minute, "hour": &job.Schedule.Hour, "dom": &job.Schedule.DOM, "month": &job.Schedule.Month, "dow": &job.Schedule.DOW} {
		v, ok := schedule[key].(string)
		if !ok || v == "" || strings.ContainsAny(v, "\x00\r\n") {
			return job, invalid
		}
		*target = v
	}
	job.Raw, _ = json.Marshal(m)
	return job, nil
}

func mutateCron(ctx context.Context, op string, id int64, config map[string]any) ([]byte, error) {
	args := make([]string, 0, 2)
	if op != "create" {
		args = append(args, strconv.FormatInt(id, 10))
	}
	if config != nil {
		b, err := json.Marshal(config)
		if err != nil {
			return nil, errors.New("cannot encode cron configuration")
		}
		args = append(args, string(b))
	}
	return callMidclt(ctx, op, args...)
}

// The installer intentionally invokes the middleware client directly, never a
// shell. Both streams and execution duration are bounded; raw stderr is discarded.
func callMidclt(ctx context.Context, op string, args ...string) ([]byte, error) {
	switch op {
	case "query", "create", "update", "delete":
	default:
		return nil, errors.New("invalid cron middleware operation")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "midclt", append([]string{"call", "cronjob." + op}, args...)...)
	var out bytes.Buffer
	stdout := &boundedCronOutput{buffer: &out, remaining: 1 << 20}
	stderr := &boundedCronOutput{remaining: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil || stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("cron middleware %s failed", op)
	}
	return out.Bytes(), nil
}

type boundedCronOutput struct {
	buffer    *bytes.Buffer
	remaining int
	exceeded  bool
}

func (w *boundedCronOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.exceeded = true
		return 0, errors.New("cron output limit exceeded")
	}
	w.remaining -= len(p)
	if w.buffer != nil {
		return w.buffer.Write(p)
	}
	return len(p), nil
}

// strictJSON rejects duplicate keys, excessive nesting and trailing values. Using
// json.Number prevents large IDs from silently rounding to a different identity.
func strictJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 32 {
			return nil, errors.New("JSON nesting limit exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				m := make(map[string]any)
				for d.More() {
					token, err := d.Token()
					if err != nil {
						return nil, err
					}
					key, ok := token.(string)
					if !ok {
						return nil, errors.New("invalid JSON key")
					}
					if _, exists := m[key]; exists {
						return nil, errors.New("duplicate JSON key")
					}
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					m[key] = value
				}
				if end, err := d.Token(); err != nil || end != json.Delim('}') {
					return nil, errors.New("invalid JSON object")
				}
				return m, nil
			case '[':
				a := make([]any, 0)
				for d.More() {
					value, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					a = append(a, value)
				}
				if end, err := d.Token(); err != nil || end != json.Delim(']') {
					return nil, errors.New("invalid JSON array")
				}
				return a, nil
			default:
				return nil, errors.New("invalid JSON delimiter")
			}
		}
		return token, nil
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

// sameCommand compares literal shell words, allowing existing jobs that use
// unquoted safe paths. Operators, expansions and substitutions are refused.
func sameCommand(a, b string) bool {
	if a == b {
		return true
	}
	x, okX := literalShellWords(a)
	y, okY := literalShellWords(b)
	return okX && okY && reflect.DeepEqual(x, y)
}

func literalShellWords(s string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if quote == '\'' {
			if ch == quote {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		if ch == '\\' {
			if i+1 == len(s) || s[i+1] == '\n' {
				return nil, false
			}
			i++
			if quote == '"' && !strings.ContainsRune("$`\"\\", rune(s[i])) {
				word.WriteByte('\\')
			}
			word.WriteByte(s[i])
			started = true
			continue
		}
		if quote == '"' {
			if ch == quote {
				quote = 0
			} else if ch == '$' || ch == '`' {
				return nil, false
			} else {
				word.WriteByte(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote, started = ch, true
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case '$', '`', ';', '|', '&', '<', '>', '(', ')', '\n', '\r', '*', '?', '[', ']', '#', '~', '{', '}':
			return nil, false
		default:
			word.WriteByte(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, true
}
