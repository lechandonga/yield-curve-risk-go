// Package curve builds discount curves from local zero-rate quotes.
//
// Conventions:
//   - Quotes are continuously compounded zero rates; DF(t) = exp(-r(t)*t).
//   - Interpolation is linear in the zero rate between adjacent nodes.
//   - Within the quoted range the curve reprices every input quote
//     exactly (self-consistent by construction).
//   - Extrapolation outside the quoted range is explicit and
//     configurable via ExtrapolationMode.
//
// A build is all-or-nothing: any invalid input or any arbitrage
// shape (non-positive implied forward between nodes) fails the whole
// build with a BuildError listing every reason.
package curve

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/quote"
)

// ExtrapolationMode controls DF queries outside the quoted tenor range.
type ExtrapolationMode int

const (
	// ExtrapolateFlatZero keeps the edge zero rate constant beyond the
	// first/last node (default, stable and monotone).
	ExtrapolateFlatZero ExtrapolationMode = iota
	// ExtrapolateError makes out-of-range queries fail loudly.
	ExtrapolateError
)

// Config controls curve construction and queries.
type Config struct {
	QuotePolicy     quote.Policy
	Extrapolation   ExtrapolationMode
	AllowDirtyBatch bool // if true, invalid quotes are dropped (with issues kept); if false, any issue fails the build
}

// Node is one curve pillar.
type Node struct {
	Tenor    float64 // years
	ZeroRate float64 // continuously compounded
}

// BuildError reports every reason a curve build failed.
type BuildError struct {
	Issues []quote.Issue
	// Structural problems found after quote validation, e.g. arbitrage.
	Structural []string
}

func (e *BuildError) Error() string {
	var b strings.Builder
	b.WriteString("curve build failed:")
	for _, i := range e.Issues {
		fmt.Fprintf(&b, "\n  - %s", i)
	}
	for _, s := range e.Structural {
		fmt.Fprintf(&b, "\n  - %s", s)
	}
	return b.String()
}

// Curve is an immutable discount curve. Safe for concurrent reads.
type Curve struct {
	nodes   []Node // sorted by tenor, strictly increasing
	cfg     Config
	quotes  []quote.Quote // the deduped quotes this curve was built from
	builtAt time.Time
}

// BuiltAt returns the build timestamp.
func (c *Curve) BuiltAt() time.Time { return c.builtAt }

// Nodes returns a copy of the curve pillars.
func (c *Curve) Nodes() []Node { return append([]Node(nil), c.nodes...) }

// Quotes returns a copy of the quotes the curve was built from.
func (c *Curve) Quotes() []quote.Quote { return append([]quote.Quote(nil), c.quotes...) }

// Build constructs a curve from raw local quotes.
// now is injected so results are reproducible in tests.
func Build(quotes []quote.Quote, cfg Config, now time.Time) (*Curve, []quote.Issue, error) {
	var allIssues []quote.Issue
	var valid []quote.Quote
	for _, q := range quotes {
		iss := quote.Validate(q, cfg.QuotePolicy, now)
		if len(iss) > 0 {
			allIssues = append(allIssues, iss...)
			continue
		}
		valid = append(valid, q)
	}
	deduped, conflictIssues := quote.Dedupe(valid, cfg.QuotePolicy)
	allIssues = append(allIssues, conflictIssues...)

	if !cfg.AllowDirtyBatch && len(allIssues) > 0 {
		return nil, allIssues, &BuildError{Issues: allIssues}
	}
	if len(deduped) == 0 {
		err := &BuildError{Issues: allIssues, Structural: []string{"no usable quotes"}}
		return nil, allIssues, err
	}

	nodes := make([]Node, len(deduped))
	for i, q := range deduped {
		t, _ := quote.ParseTenor(q.Tenor) // already validated
		nodes[i] = Node{Tenor: t, ZeroRate: q.Mid()}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Tenor < nodes[j].Tenor })

	// Arbitrage check: discount factors must be strictly decreasing
	// across nodes, i.e. every implied forward rate must be positive.
	var structural []string
	prevDF := 1.0
	prevT := 0.0
	for _, n := range nodes {
		df := math.Exp(-n.ZeroRate * n.Tenor)
		if df >= prevDF {
			fwd := math.Log(prevDF/df) / (n.Tenor - prevT)
			structural = append(structural,
				fmt.Sprintf("non-positive implied forward %.6f between t=%g and t=%g (arbitrage shape)",
					fwd, prevT, n.Tenor))
		}
		prevDF, prevT = df, n.Tenor
	}
	if len(structural) > 0 {
		return nil, allIssues, &BuildError{Issues: allIssues, Structural: structural}
	}

	return &Curve{nodes: nodes, cfg: cfg, quotes: deduped, builtAt: now}, allIssues, nil
}

// ZeroRate returns the interpolated continuously compounded zero rate at t.
func (c *Curve) ZeroRate(t float64) (float64, error) {
	if t <= 0 {
		return 0, fmt.Errorf("tenor %g must be positive", t)
	}
	n := len(c.nodes)
	first, last := c.nodes[0], c.nodes[n-1]
	switch {
	case t < first.Tenor:
		if c.cfg.Extrapolation == ExtrapolateError {
			return 0, fmt.Errorf("t=%g below first node %g and extrapolation is disabled", t, first.Tenor)
		}
		return first.ZeroRate, nil
	case t > last.Tenor:
		if c.cfg.Extrapolation == ExtrapolateError {
			return 0, fmt.Errorf("t=%g beyond last node %g and extrapolation is disabled", t, last.Tenor)
		}
		return last.ZeroRate, nil
	}
	// Binary search for the bracketing interval.
	i := sort.Search(n, func(i int) bool { return c.nodes[i].Tenor >= t })
	if c.nodes[i].Tenor == t {
		return c.nodes[i].ZeroRate, nil
	}
	a, b := c.nodes[i-1], c.nodes[i]
	w := (t - a.Tenor) / (b.Tenor - a.Tenor)
	return a.ZeroRate + w*(b.ZeroRate-a.ZeroRate), nil
}

// DiscountFactor returns DF(t) for any positive t.
func (c *Curve) DiscountFactor(t float64) (float64, error) {
	r, err := c.ZeroRate(t)
	if err != nil {
		return 0, err
	}
	return math.Exp(-r * t), nil
}

// RepriceQuotes reprices every input quote off the built curve and
// returns the maximum absolute error. Self-consistency requires this
// to be ~0 (exact by construction, modulo float rounding).
func (c *Curve) RepriceQuotes() float64 {
	maxErr := 0.0
	for _, q := range c.quotes {
		t, _ := quote.ParseTenor(q.Tenor)
		r, err := c.ZeroRate(t)
		if err != nil {
			return math.Inf(1)
		}
		if d := math.Abs(r - q.Mid()); d > maxErr {
			maxErr = d
		}
	}
	return maxErr
}

// Shifted returns a new curve with per-node zero-rate shifts applied.
// shifts must have exactly one entry per node, in node order.
func (c *Curve) Shifted(shifts []float64) (*Curve, error) {
	if len(shifts) != len(c.nodes) {
		return nil, fmt.Errorf("shift count %d != node count %d", len(shifts), len(c.nodes))
	}
	nodes := make([]Node, len(c.nodes))
	copy(nodes, c.nodes)
	for i := range nodes {
		nodes[i].ZeroRate += shifts[i]
	}
	return &Curve{nodes: nodes, cfg: c.cfg, quotes: c.quotes, builtAt: c.builtAt}, nil
}
