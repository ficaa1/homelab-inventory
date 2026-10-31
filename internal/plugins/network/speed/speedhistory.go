package speed

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

// speedHistory keeps the newest runs in memory and rewrites the whole JSONL
// file on every append. At 50 short lines a rewrite is cheaper than reasoning
// about partial appends, and the temp-file rename keeps readers from seeing a
// half-written file.
type speedHistory struct {
	mu   sync.Mutex
	path string
	runs []SpeedResult
}

func openSpeedHistory(path string) (*speedHistory, error) {
	h := &speedHistory{path: path}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Bound both corrupt-line storage and history memory while loading.
	reader := bufio.NewReaderSize(f, 64<<10)
	oversized := false
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			oversized = true
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if !oversized {
			var r SpeedResult
			if json.Unmarshal(line, &r) == nil && !r.Time.IsZero() {
				if len(h.runs) == speedHistoryMax {
					copy(h.runs, h.runs[1:])
					h.runs = h.runs[:speedHistoryMax-1]
				}
				h.runs = append(h.runs, r)
			}
		}
		oversized = false
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return h, nil
}

func (h *speedHistory) Append(r SpeedResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.runs
	h.runs = append(append([]SpeedResult(nil), h.runs...), r)
	if over := len(h.runs) - speedHistoryMax; over > 0 {
		h.runs = h.runs[over:]
	}
	if err := h.writeLocked(); err != nil {
		h.runs = previous
		return err
	}
	return nil
}

func (h *speedHistory) Finish(r SpeedResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.runs) - 1; i >= 0; i-- {
		if h.runs[i].Time.Equal(r.Time) {
			h.runs[i] = r
			if err := h.writeLocked(); err != nil {
				// Keep the measured result visible, but explicitly disclose non-durability.
				h.runs[i].Error += "; history not saved"
				return err
			}
			return nil
		}
	}
	return errors.New("speedtest start missing")
}

// Restore cooldown from the newest start, even if file timestamps are unordered.
func (h *speedHistory) LatestStart() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	var latest time.Time
	for _, r := range h.runs {
		if r.Time.After(latest) {
			latest = r.Time
		}
	}
	return latest
}

// List returns runs newest first.
func (h *speedHistory) List() []SpeedResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SpeedResult, len(h.runs))
	for i, r := range h.runs {
		out[len(h.runs)-1-i] = r
	}
	return out
}

func (h *speedHistory) writeLocked() error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range h.runs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return fsx.WriteAtomic(h.path, buf.Bytes(), 0o600)
}
