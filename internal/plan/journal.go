package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Journal persists the latest run of one (kind, key) operation under
// $XDG_STATE_HOME/lazylongarm/ops/. It holds step statuses and non-secret
// summary values only; secrets are never written.
type Journal struct {
	mu   sync.Mutex
	path string
	rec  Record
	// ForceResume ignores a previous unknown outcome (the user inspected it).
	ForceResume bool
}

// Record is the on-disk form.
type Record struct {
	Kind     string            `json:"kind"`
	Key      string            `json:"key"`
	Title    string            `json:"title"`
	Digest   string            `json:"digest"`
	Summary  map[string]string `json:"summary"`
	Started  time.Time         `json:"started"`
	Updated  time.Time         `json:"updated"`
	Results  []Result          `json:"results"`
	Finished bool              `json:"finished"`
}

// OpenJournal loads (or prepares) the journal for kind/key in dir.
func OpenJournal(dir, kind, key string) *Journal {
	name := sanitizeName(kind) + "--" + sanitizeName(key) + ".json"
	j := &Journal{path: filepath.Join(dir, "ops", name)}
	if data, err := os.ReadFile(j.path); err == nil {
		_ = json.Unmarshal(data, &j.rec)
	}
	return j
}

// Path of the journal file.
func (j *Journal) Path() string { return j.path }

// Previous returns the last recorded run (zero Record when none).
func (j *Journal) Previous() Record { j.mu.Lock(); defer j.mu.Unlock(); return j.rec }

// BlockingUnknown names a non-idempotent step that a previous run left in an
// unknown state, unless ForceResume.
func (j *Journal) BlockingUnknown(p *Plan) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.ForceResume {
		return ""
	}
	idem := map[string]bool{}
	for _, s := range p.Steps {
		idem[s.ID] = s.Idempotent
	}
	for _, r := range j.rec.Results {
		if r.Status == Unknown && !idem[r.ID] {
			return r.ID
		}
	}
	return ""
}

// Begin starts a new record (the previous file is kept as .prev).
func (j *Journal) Begin(p *Plan) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := os.Stat(j.path); err == nil {
		_ = os.Rename(j.path, j.path+".prev")
	}
	summary := map[string]string{}
	for _, kv := range p.Summary {
		summary[kv[0]] = kv[1]
	}
	now := time.Now().UTC()
	j.rec = Record{Kind: p.Kind, Key: p.Key, Title: p.Title, Digest: p.Digest(), Summary: summary, Started: now, Updated: now}
	j.writeLocked()
}

// Update records step results.
func (j *Journal) Update(results []Result) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.rec.Results = append([]Result(nil), results...)
	j.rec.Updated = time.Now().UTC()
	done := true
	for _, r := range results {
		if r.Status != Done && r.Status != Skipped && r.Status != Verified {
			done = false
		}
	}
	j.rec.Finished = done
	j.writeLocked()
}

func (j *Journal) writeLocked() {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(j.rec, "", "  ")
	if err != nil {
		return
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, j.path)
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "local"
	}
	return b.String()
}
