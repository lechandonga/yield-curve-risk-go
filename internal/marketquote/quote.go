// Package marketquote defines locally simulated market quotes, deterministic
// validation rules and quote-set preparation for curve construction.
//
// No network or external service is involved anywhere in this package: every
// input quote is supplied by the caller (see the test fixtures and demo).
package marketquote

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// InstrumentKind identifies how a quoted rate is interpreted during bootstrap.
type InstrumentKind string

const (
	// Deposit is a simple zero-coupon deposit: the quoted mid rate is the
	// continuously compounded zero rate for TenorYears, i.e. DF(t)=exp(-r*t).
	Deposit InstrumentKind = "DEPOSIT"
	// Swap is an annual-fixed par interest-rate swap. Its quoted mid rate s
	// satisfies 1 = s*sum_k DF(T_k) + DF(T_n) over annual payment dates.
	Swap InstrumentKind = "SWAP"
)

// RejectReason explains why a quote was rejected. Values are stable strings
// so callers can branch on them.
type RejectReason string

const (
	MissingID      RejectReason = "MISSING_ID"
	UnknownKind    RejectReason = "UNKNOWN_KIND"
	InvalidTenor   RejectReason = "INVALID_TENOR"
	NonFiniteRate  RejectReason = "NON_FINITE_RATE"
	NegativeSpread RejectReason = "NEGATIVE_SPREAD"
	InvertedQuote  RejectReason = "INVERTED_QUOTE"
	StaleQuote     RejectReason = "STALE_QUOTE"
	FutureQuote    RejectReason = "FUTURE_QUOTE"
	DuplicateID    RejectReason = "DUPLICATE_ID"
	DuplicateQuote RejectReason = "DUPLICATE_QUOTE"
	ConflictQuote  RejectReason = "CONFLICT_QUOTE"
)

// Fatal reports whether a reject reason aborts the whole batch.
//
// Deterministic policy:
//   - An exact duplicate (same key, same prices/time) is non-fatal: the first
//     occurrence under the fixed (QuoteTime, ID) ordering wins and the extra
//     copy is reported. Nothing is chosen silently.
//   - A conflicting quote (same key, different prices) is fatal because any
//     automatic choice would be arbitrary.
//   - Every data-quality defect (bad fields, stale/inverted quotes) is fatal:
//     a curve must never be bootstrapped from polluted input.
func (r RejectReason) Fatal() bool {
	return r != DuplicateQuote
}

// Quote is a single local two-way rate quote.
// Rates are decimal (0.03 means 3%), BidRate <= AskRate is required.
type Quote struct {
	ID         string
	Kind       InstrumentKind
	TenorYears float64
	BidRate    float64
	AskRate    float64
	QuoteTime  time.Time
}

// Mid returns the deterministic mid used by the curve builder.
func (q Quote) Mid() float64 { return (q.BidRate + q.AskRate) / 2 }

// key is the uniqueness dimension for a quote.
func (q Quote) key() string { return fmt.Sprintf("%s@%.12g", q.Kind, q.TenorYears) }

// Policy configures quote acceptance rules.
type Policy struct {
	// MaxAge rejects quotes whose QuoteTime is older than AsOf-MaxAge.
	MaxAge time.Duration
	// FutureTolerance rejects quotes timestamped more than this after AsOf.
	FutureTolerance time.Duration
}

// DefaultPolicy is the standard acceptance policy.
func DefaultPolicy() Policy {
	return Policy{MaxAge: 15 * time.Minute, FutureTolerance: 30 * time.Second}
}

// RejectRecord ties a rejected quote to an explainable reason.
type RejectRecord struct {
	Quote  Quote
	Reason RejectReason
	Detail string
}

func (r RejectRecord) String() string {
	return fmt.Sprintf("quote %q (%s t=%.4g): %s %s",
		r.Quote.ID, r.Quote.Kind, r.Quote.TenorYears, r.Reason, r.Detail)
}

// PreparedSet is the deterministic, validated output for a batch.
type PreparedSet struct {
	AsOf     time.Time
	Deposits []Quote
	Swaps    []Quote
	Rejected []RejectRecord
}

// Validate performs field validation, staleness checks and duplicate/conflict
// resolution with fully deterministic rules. It returns:
//   - prepared: the cleaned quote set (only meaningful when err == nil)
//   - rejected: every excluded quote with its distinguishable reason
//   - err: non-nil (a *BatchError) whenever the batch cannot be used.
func Validate(quotes []Quote, asOf time.Time, policy Policy) (*PreparedSet, []RejectRecord, error) {
	rejected := make([]RejectRecord, 0)
	addReject := func(q Quote, reason RejectReason, format string, args ...any) {
		rejected = append(rejected, RejectRecord{
			Quote: q, Reason: reason, Detail: strings.TrimSpace(fmt.Sprintf(format, args...)),
		})
	}

	// Deterministic processing order: QuoteTime ascending, then ID. This makes
	// duplicate resolution ("first wins") reproducible regardless of input
	// slice order.
	ordered := make([]Quote, len(quotes))
	copy(ordered, quotes)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].QuoteTime.Equal(ordered[j].QuoteTime) {
			return ordered[i].QuoteTime.Before(ordered[j].QuoteTime)
		}
		return ordered[i].ID < ordered[j].ID
	})

	seenID := make(map[string]int)
	byKey := make(map[string]Quote)
	var deposits, swaps []Quote

	for _, q := range ordered {
		if strings.TrimSpace(q.ID) == "" {
			addReject(q, MissingID, "quote identifier is empty")
			continue
		}
		switch q.Kind {
		case Deposit, Swap:
		default:
			addReject(q, UnknownKind, "kind %q is not supported", q.Kind)
			continue
		}
		if math.IsNaN(q.TenorYears) || math.IsInf(q.TenorYears, 0) || q.TenorYears <= 0 {
			addReject(q, InvalidTenor, "tenor must be a finite positive number of years, got %v", q.TenorYears)
			continue
		}
		switch {
		case math.IsNaN(q.BidRate) || math.IsInf(q.BidRate, 0),
			math.IsNaN(q.AskRate) || math.IsInf(q.AskRate, 0):
			addReject(q, NonFiniteRate, "bid/ask must be finite, got bid=%v ask=%v", q.BidRate, q.AskRate)
			continue
		case q.BidRate < 0 || q.AskRate < 0:
			addReject(q, NegativeSpread, "negative bid/ask is not accepted bid=%v ask=%v", q.BidRate, q.AskRate)
			continue
		case q.BidRate > q.AskRate:
			addReject(q, InvertedQuote, "bid %.6f is above ask %.6f", q.BidRate, q.AskRate)
			continue
		}
		if policy.MaxAge > 0 && asOf.Sub(q.QuoteTime) > policy.MaxAge {
			addReject(q, StaleQuote, "quote time %s is older than max age %s relative to as-of %s",
				q.QuoteTime.Format(time.RFC3339), policy.MaxAge, asOf.Format(time.RFC3339))
			continue
		}
		if q.QuoteTime.Sub(asOf) > policy.FutureTolerance {
			addReject(q, FutureQuote, "quote time %s is more than %s after as-of %s",
				q.QuoteTime.Format(time.RFC3339), policy.FutureTolerance, asOf.Format(time.RFC3339))
			continue
		}

		if existing, ok := byKey[q.key()]; ok {
			if existing.BidRate == q.BidRate && existing.AskRate == q.AskRate && existing.QuoteTime.Equal(q.QuoteTime) {
				// Non-fatal: first occurrence under fixed ordering wins; still reported.
				addReject(q, DuplicateQuote, "exact duplicate of %q, first occurrence kept", existing.ID)
				continue
			}
			addReject(q, ConflictQuote,
				"key %s conflicts with kept quote %q (bid/ask %v/%v at %s vs %v/%v at %s)",
				q.key(), existing.ID, existing.BidRate, existing.AskRate, existing.QuoteTime,
				q.BidRate, q.AskRate, q.QuoteTime)
			continue
		}

		if _, ok := seenID[q.ID]; ok {
			addReject(q, DuplicateID, "identifier %q is used more than once", q.ID)
			continue
		}
		seenID[q.ID] = len(seenID)
		byKey[q.key()] = q
		if q.Kind == Deposit {
			deposits = append(deposits, q)
		} else {
			swaps = append(swaps, q)
		}
	}

	prepared := &PreparedSet{AsOf: asOf, Deposits: deposits, Swaps: swaps, Rejected: rejected}
	var fatal []RejectRecord
	for _, rec := range rejected {
		if rec.Reason.Fatal() {
			fatal = append(fatal, rec)
		}
	}
	if len(fatal) > 0 {
		return prepared, rejected, &BatchError{Rejects: fatal}
	}
	return prepared, rejected, nil
}

// BatchError is returned when one or more fatal defects make a batch unusable.
type BatchError struct {
	Rejects []RejectRecord
}

func (e *BatchError) Error() string {
	parts := make([]string, 0, len(e.Rejects))
	for _, r := range e.Rejects {
		parts = append(parts, r.String())
	}
	return fmt.Sprintf("quote batch rejected with %d fatal defect(s): %s", len(e.Rejects), strings.Join(parts, "; "))
}
