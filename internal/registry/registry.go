// Package registry provides online quote updates and atomic curve version
// management.
//
// Consistency model:
//   - Versions are immutable once published; readers always observe either a
//     complete old version or a complete new version, never a half-built one.
//   - Update is all-or-nothing: validation, bootstrap and self-checks run
//     off-lock. Only after a full curve is built is the publish lock taken.
//   - Concurrent updates are serialised through the publish mutex (bounded
//     wait); a batch that fails validation/build leaves the last good version
//     untouched and returns an UpdateError describing every fatal defect.
//   - Current reads are lock-free (atomic pointer). Historical lookups take a
//     cheap RLock and observe the committed append-only history.
package registry

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

// Version is one immutable published curve generation.
type Version struct {
	Number    int
	Curve     *curve.Curve
	AsOf      time.Time
	Quotes    []marketquote.Quote
	Warnings  []marketquote.RejectRecord // non-fatal records (e.g. duplicates)
	CreatedAt time.Time
}

// Config bundles the policies used for every update.
type Config struct {
	Quote marketquote.Policy
	Curve curve.Config
}

// DefaultConfig returns default quote and curve policies.
func DefaultConfig() Config {
	return Config{Quote: marketquote.DefaultPolicy(), Curve: curve.DefaultConfig()}
}

// UpdateError exposes why an update was rejected; the last good version
// remains current.
type UpdateError struct {
	Rejects  []marketquote.RejectRecord
	BuildErr error
}

func (e *UpdateError) Error() string {
	if e.BuildErr != nil {
		return fmt.Sprintf("registry: curve build failed, previous version retained: %v", e.BuildErr)
	}
	return fmt.Sprintf("registry: quote batch rejected (%d fatal defect(s)), previous version retained", len(e.Rejects))
}

// Registry owns the current version and the immutable version history.
// The zero value is not usable; use New.
type Registry struct {
	cfg Config

	publishMu sync.Mutex
	current   atomic.Pointer[Version]

	historyMu sync.RWMutex
	history   []*Version // append-only, indexed by Number-1
}

// New creates a registry. seed may be nil (no current version until the first
// successful update).
func New(cfg Config, seed *Version) *Registry {
	r := &Registry{cfg: cfg}
	if seed != nil {
		r.current.Store(seed)
		r.history = append(r.history, seed)
	}
	return r
}

// Update validates a full quote batch, builds a curve, and publishes a new
// version atomically. On any failure the last good version is kept.
func (r *Registry) Update(quotes []marketquote.Quote, asOf time.Time) (*Version, error) {
	prepared, rejects, err := marketquote.Validate(quotes, asOf, r.cfg.Quote)
	if err != nil {
		return nil, &UpdateError{Rejects: fatalRejects(rejects), BuildErr: err}
	}
	c, err := curve.Build(prepared, r.cfg.Curve)
	if err != nil {
		return nil, &UpdateError{BuildErr: err}
	}

	r.publishMu.Lock()
	defer r.publishMu.Unlock()

	next := &Version{
		Number:    r.nextNumberLocked(),
		Curve:     c,
		AsOf:      asOf,
		Quotes:    append([]marketquote.Quote(nil), quotes...),
		Warnings:  nonFatalRejects(rejects),
		CreatedAt: time.Now(),
	}
	r.historyMu.Lock()
	r.history = append(r.history, next)
	// Publish current only after history commit, all under the publish
	// mutex: a reader that observes version N can always look N up.
	r.current.Store(next)
	r.historyMu.Unlock()
	return next, nil
}

func (r *Registry) nextNumberLocked() int {
	if cur := r.current.Load(); cur != nil {
		return cur.Number + 1
	}
	return 1
}

// Current returns the current published version, or nil if none exists yet.
func (r *Registry) Current() *Version { return r.current.Load() }

// Get returns an immutable historical version by number.
func (r *Registry) Get(number int) (*Version, error) {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	if number < 1 || number > len(r.history) {
		return nil, fmt.Errorf("registry: version %d does not exist (have %d versions)", number, len(r.history))
	}
	return r.history[number-1], nil
}

// Versions returns a snapshot of published version metadata (numbers and
// as-of timestamps), oldest first.
func (r *Registry) Versions() []struct {
	Number int
	AsOf   time.Time
} {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	out := make([]struct {
		Number int
		AsOf   time.Time
	}, len(r.history))
	for i, v := range r.history {
		out[i].Number = v.Number
		out[i].AsOf = v.AsOf
	}
	return out
}

func fatalRejects(all []marketquote.RejectRecord) []marketquote.RejectRecord {
	var out []marketquote.RejectRecord
	for _, rec := range all {
		if rec.Reason.Fatal() {
			out = append(out, rec)
		}
	}
	return out
}

func nonFatalRejects(all []marketquote.RejectRecord) []marketquote.RejectRecord {
	var out []marketquote.RejectRecord
	for _, rec := range all {
		if !rec.Reason.Fatal() {
			out = append(out, rec)
		}
	}
	return out
}
