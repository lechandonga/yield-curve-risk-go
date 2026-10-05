// Package engine manages versioned curves under concurrent
// online quote updates.
//
// Consistency model:
//   - Readers always see one complete immutable version: either the
//     full old curve or the full new curve, never a half-built state.
//     The current version is swapped via atomic.Pointer.
//   - Updates are serialized by a mutex; a batch is validated and
//     built fully before publishing. If the build fails, the previous
//     version stays current and the failure (with all reasons) is
//     returned and recorded in the version history.
//   - History lookups are protected by an RWMutex and return
//     immutable snapshots, so concurrent readers/updaters cannot
//     race, deadlock, or observe contradictory results.
package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/curve"
	"github.com/lechandonga/yield-curve-risk-go/quote"
)

// Version is an immutable, published curve snapshot.
type Version struct {
	ID       int64
	Curve    *curve.Curve
	BuiltAt  time.Time
	Issues   []quote.Issue // non-fatal issues tolerated during the build (dirty batch mode)
	QuoteNum int
}

// Failure records a rejected update batch.
type Failure struct {
	At     time.Time
	Reason error
}

// Engine is the concurrency-safe curve service.
type Engine struct {
	cfg     curve.Config
	nowFunc func() time.Time // injectable clock for reproducibility

	current atomic.Pointer[Version]

	mu      sync.Mutex // serializes updates
	histMu  sync.RWMutex
	history map[int64]*Version
	nextID  int64

	failMu   sync.RWMutex
	failures []Failure
}

// New creates an engine. nowFunc may be nil (uses time.Now).
func New(cfg curve.Config, nowFunc func() time.Time) *Engine {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Engine{
		cfg:     cfg,
		nowFunc: nowFunc,
		history: map[int64]*Version{},
	}
}

// Update validates and builds a curve from a quote batch, then
// publishes it atomically. On any build failure the previous version
// remains current and the error (with all reasons) is returned.
func (e *Engine) Update(quotes []quote.Quote) (*Version, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	c, issues, err := curve.Build(quotes, e.cfg, e.nowFunc())
	if err != nil {
		e.failMu.Lock()
		e.failures = append(e.failures, Failure{At: e.nowFunc(), Reason: err})
		e.failMu.Unlock()
		return nil, err
	}

	e.histMu.Lock()
	e.nextID++
	v := &Version{
		ID:       e.nextID,
		Curve:    c,
		BuiltAt:  c.BuiltAt(),
		Issues:   issues,
		QuoteNum: len(c.Quotes()),
	}
	e.history[v.ID] = v
	e.histMu.Unlock()

	e.current.Store(v) // single atomic publish
	return v, nil
}

// Current returns the latest published version, or nil if none.
// The returned pointer is immutable and safe to use concurrently.
func (e *Engine) Current() *Version { return e.current.Load() }

// Version returns a historical version by ID.
func (e *Engine) Version(id int64) (*Version, bool) {
	e.histMu.RLock()
	defer e.histMu.RUnlock()
	v, ok := e.history[id]
	return v, ok
}

// Versions lists all published version IDs in ascending order.
func (e *Engine) Versions() []int64 {
	e.histMu.RLock()
	defer e.histMu.RUnlock()
	ids := make([]int64, 0, len(e.history))
	for id := range e.history {
		ids = append(ids, id)
	}
	for i := 0; i < len(ids); i++ { // insertion-sort free deterministic order
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
	return ids
}

// Failures returns a copy of all rejected update attempts.
func (e *Engine) Failures() []Failure {
	e.failMu.RLock()
	defer e.failMu.RUnlock()
	return append([]Failure(nil), e.failures...)
}

// String summarizes the engine state.
func (e *Engine) String() string {
	cur := e.Current()
	if cur == nil {
		return "engine: no version published"
	}
	return fmt.Sprintf("engine: current version %d built at %s from %d quotes",
		cur.ID, cur.BuiltAt.Format(time.RFC3339), cur.QuoteNum)
}
