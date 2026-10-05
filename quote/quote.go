// Package quote defines the local quote model and deterministic
// dirty-data handling rules.
//
// All rules are deterministic: the same quote set always yields the
// same validation result, at any time and under any concurrency.
// No invalid quote is silently dropped or silently picked; every
// rejection carries a distinguishable reason code.
package quote

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReasonCode distinguishes why a quote was rejected.
type ReasonCode string

const (
	ReasonMissingField   ReasonCode = "MISSING_FIELD"   // required field missing
	ReasonInvalidTenor   ReasonCode = "INVALID_TENOR"   // tenor unparsable or non-positive
	ReasonInvertedMarket ReasonCode = "INVERTED_MARKET" // bid > ask
	ReasonStale          ReasonCode = "STALE"           // market data too old
	ReasonConflict       ReasonCode = "CONFLICT"        // conflicting quotes at same tenor
)

// Issue is a deterministic validation finding on one quote.
type Issue struct {
	QuoteID string
	Reason  ReasonCode
	Detail  string
}

func (i Issue) Error() string {
	return fmt.Sprintf("quote %q: %s: %s", i.QuoteID, i.Reason, i.Detail)
}

// Quote is one local rate quote in continuously compounded zero-rate terms.
type Quote struct {
	ID        string
	Tenor     string // e.g. "6M", "5Y"
	Bid       float64
	Ask       float64
	Timestamp time.Time
}

// Mid returns the bid/ask midpoint.
func (q Quote) Mid() float64 { return (q.Bid + q.Ask) / 2 }

// ParseTenor parses "3M"/"1Y"/"2W"/"30D" into years (ACT/365).
// Invalid tenors return an error; nothing is guessed silently.
func ParseTenor(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if len(s) < 2 {
		return 0, fmt.Errorf("tenor %q too short", s)
	}
	unit := s[len(s)-1]
	var num float64
	if _, err := fmt.Sscanf(s[:len(s)-1], "%g", &num); err != nil {
		return 0, fmt.Errorf("tenor %q: bad number: %v", s, err)
	}
	if num <= 0 {
		return 0, fmt.Errorf("tenor %q: non-positive length", s)
	}
	switch unit {
	case 'D':
		return num / 365, nil
	case 'W':
		return num * 7 / 365, nil
	case 'M':
		return num / 12, nil
	case 'Y':
		return num, nil
	default:
		return 0, fmt.Errorf("tenor %q: unknown unit %q", s, string(unit))
	}
}

// Policy controls quote validation.
type Policy struct {
	// MaxAge is the maximum allowed quote age; <=0 disables staleness checks.
	MaxAge time.Duration
	// ConflictTolerance is the max mid difference at one tenor that is
	// still treated as a duplicate rather than a conflict.
	ConflictTolerance float64
}

// Validate checks a single quote and returns all findings (possibly many).
func Validate(q Quote, pol Policy, now time.Time) []Issue {
	var issues []Issue
	if q.ID == "" {
		issues = append(issues, Issue{q.ID, ReasonMissingField, "empty quote ID"})
	}
	if q.Tenor == "" {
		issues = append(issues, Issue{q.ID, ReasonMissingField, "empty tenor"})
	} else if _, err := ParseTenor(q.Tenor); err != nil {
		issues = append(issues, Issue{q.ID, ReasonInvalidTenor, err.Error()})
	}
	if q.Timestamp.IsZero() {
		issues = append(issues, Issue{q.ID, ReasonMissingField, "zero timestamp"})
	} else if pol.MaxAge > 0 && now.Sub(q.Timestamp) > pol.MaxAge {
		issues = append(issues, Issue{q.ID, ReasonStale,
			fmt.Sprintf("age %s exceeds max %s", now.Sub(q.Timestamp).Round(time.Second), pol.MaxAge)})
	}
	if q.Bid > q.Ask {
		issues = append(issues, Issue{q.ID, ReasonInvertedMarket,
			fmt.Sprintf("bid %g > ask %g", q.Bid, q.Ask)})
	}
	return issues
}

// Dedupe merges field-valid quotes by tenor with fully deterministic rules:
//   - mids within ConflictTolerance at one tenor: duplicates; keep the
//     newest timestamp (ties broken by smallest ID);
//   - mids differing by more than ConflictTolerance: conflict; the whole
//     tenor is rejected with CONFLICT issues on every involved quote.
//
// Returns usable quotes sorted by tenor ascending, plus all issues.
func Dedupe(quotes []Quote, pol Policy) ([]Quote, []Issue) {
	byTenor := map[float64][]Quote{}
	tenorOf := map[float64]string{}
	for _, q := range quotes {
		t, err := ParseTenor(q.Tenor)
		if err != nil {
			continue // field-level issue already reported by Validate
		}
		byTenor[t] = append(byTenor[t], q)
		tenorOf[t] = q.Tenor
	}
	var tenors []float64
	for t := range byTenor {
		tenors = append(tenors, t)
	}
	sort.Float64s(tenors)

	var out []Quote
	var issues []Issue
	for _, t := range tenors {
		group := byTenor[t]
		// Deterministic order: newest timestamp first, then smallest ID.
		sort.Slice(group, func(i, j int) bool {
			if !group[i].Timestamp.Equal(group[j].Timestamp) {
				return group[i].Timestamp.After(group[j].Timestamp)
			}
			return group[i].ID < group[j].ID
		})
		best := group[0]
		conflict := false
		for _, other := range group[1:] {
			if abs(other.Mid()-best.Mid()) > pol.ConflictTolerance {
				conflict = true
			}
		}
		if conflict {
			for _, q := range group {
				issues = append(issues, Issue{q.ID, ReasonConflict,
					fmt.Sprintf("conflicting mids at tenor %s (mid %g vs %g)",
						tenorOf[t], q.Mid(), best.Mid())})
			}
			continue
		}
		out = append(out, best)
	}
	return out, issues
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
