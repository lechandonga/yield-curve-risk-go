// Package curve builds a continuously-compounded discount curve from local
// deposit/par-swap quotes and provides discount-factor queries, deterministic
// interpolation/extrapolation rules and self-consistency checks.
package curve

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

// InterpolationMethod defines zero-rate interpolation between bootstrapped nodes.
type InterpolationMethod string

const (
	// LinearZero interpolates zero rates linearly in time; this is the default.
	LinearZero InterpolationMethod = "LINEAR_ZERO"
	// FlatZero holds the left node's zero rate until the next node.
	FlatZero InterpolationMethod = "FLAT_ZERO"
)

// ExtrapolationMethod defines behaviour outside the quoted tenor range.
type ExtrapolationMethod string

const (
	// FlatExtrap holds the boundary zero rate constant. This makes long-end
	// behaviour explicit and stable (the forward flattens at the edge rate).
	FlatExtrap ExtrapolationMethod = "FLAT"
	// FailExtrap makes out-of-range queries return an error.
	FailExtrap ExtrapolationMethod = "FAIL"
)

// Config controls construction, extrapolation and self-check tolerances.
type Config struct {
	Interpolation InterpolationMethod
	Extrapolation ExtrapolationMethod
	// RepriceTol bounds the absolute re-pricing residual (rate space for
	// deposits, PV-of-1-notional space for swaps). Default 1e-10.
	RepriceTol float64
	// ArbTol bounds violations such as non-decreasing discount factors.
	ArbTol float64
	// CheckDensity is the number of dense-grid points per year used for the
	// in-range no-arbitrage shape scan. Default 52 (weekly).
	CheckDensity int
}

// DefaultConfig returns the standard configuration.
func DefaultConfig() Config {
	return Config{
		Interpolation: LinearZero,
		Extrapolation: FlatExtrap,
		RepriceTol:    1e-10,
		ArbTol:        1e-10,
		CheckDensity:  52,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Interpolation == "" {
		c.Interpolation = d.Interpolation
	}
	if c.Extrapolation == "" {
		c.Extrapolation = d.Extrapolation
	}
	if c.RepriceTol <= 0 {
		c.RepriceTol = d.RepriceTol
	}
	if c.ArbTol <= 0 {
		c.ArbTol = d.ArbTol
	}
	if c.CheckDensity <= 0 {
		c.CheckDensity = d.CheckDensity
	}
	return c
}

type zeroNode struct {
	t    float64
	zero float64
}

// Curve is an immutable, queryable discount curve.
type Curve struct {
	nodes  []zeroNode // strictly ascending t, starts at t=0 (zero=0)
	cfg    Config
	asOf   time.Time
	quotes []marketquote.Quote // every quote that produced this curve
}

// AsOf returns the curve's valuation timestamp.
func (c *Curve) AsOf() time.Time { return c.asOf }

// TenorRange returns the shortest and longest quoted tenors.
func (c *Curve) TenorRange() (float64, float64) {
	return c.nodes[1].t, c.nodes[len(c.nodes)-1].t
}

// Nodes returns a copy of the bootstrapped zero-rate nodes (t, zero rate).
func (c *Curve) Nodes() []struct{ T, Zero float64 } {
	out := make([]struct{ T, Zero float64 }, len(c.nodes))
	for i, n := range c.nodes {
		out[i].T, out[i].Zero = n.t, n.zero
	}
	return out
}

// ZeroRate returns the continuously compounded zero rate for tenor t.
func (c *Curve) ZeroRate(t float64) (float64, error) {
	if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 {
		return 0, fmt.Errorf("curve: tenor must be a finite non-negative number, got %v", t)
	}
	if t == 0 {
		return 0, nil
	}
	first := c.nodes[1].t
	last := c.nodes[len(c.nodes)-1].t
	if t < first || t > last {
		if c.cfg.Extrapolation == FailExtrap {
			return 0, fmt.Errorf("curve: tenor %.6g is outside quoted range [%.6g, %.6g] and extrapolation is FAIL",
				t, first, last)
		}
		if t < first {
			return c.nodes[1].zero, nil
		}
		return c.nodes[len(c.nodes)-1].zero, nil
	}
	idx := sort.Search(len(c.nodes), func(i int) bool { return c.nodes[i].t >= t })
	if c.nodes[idx].t == t {
		return c.nodes[idx].zero, nil
	}
	left, right := c.nodes[idx-1], c.nodes[idx]
	if c.cfg.Interpolation == FlatZero {
		return left.zero, nil
	}
	w := (t - left.t) / (right.t - left.t)
	return left.zero + w*(right.zero-left.zero), nil
}

// DF returns the discount factor DF(t) = exp(-z(t)*t).
func (c *Curve) DF(t float64) (float64, error) {
	z, err := c.ZeroRate(t)
	if err != nil {
		return 0, err
	}
	return math.Exp(-z * t), nil
}

// MustDF is the panicking convenience for tenors known to be in range.
func (c *Curve) MustDF(t float64) float64 {
	df, err := c.DF(t)
	if err != nil {
		panic(err)
	}
	return df
}

// Discounter is the minimal interface used by pricing and risk routines.
type Discounter interface {
	DF(t float64) (float64, error)
}

// Build bootstraps a curve from a validated quote set and refuses to return
// any curve that fails the self-consistency checks.
func Build(ps *marketquote.PreparedSet, cfg Config) (*Curve, error) {
	if ps == nil {
		return nil, fmt.Errorf("curve: nil prepared quote set")
	}
	cfg = cfg.withDefaults()
	if cfg.Interpolation != LinearZero && cfg.Interpolation != FlatZero {
		return nil, fmt.Errorf("curve: unsupported interpolation %q", cfg.Interpolation)
	}
	if cfg.Extrapolation != FlatExtrap && cfg.Extrapolation != FailExtrap {
		return nil, fmt.Errorf("curve: unsupported extrapolation %q", cfg.Extrapolation)
	}
	if len(ps.Deposits) == 0 {
		return nil, fmt.Errorf("curve: at least one DEPOSIT quote is required as the short anchor")
	}

	deposits := append([]marketquote.Quote(nil), ps.Deposits...)
	swaps := append([]marketquote.Quote(nil), ps.Swaps...)
	sort.Slice(deposits, func(i, j int) bool { return deposits[i].TenorYears < deposits[j].TenorYears })
	sort.Slice(swaps, func(i, j int) bool { return swaps[i].TenorYears < swaps[j].TenorYears })

	nodes := []zeroNode{{t: 0, zero: 0}}
	for _, q := range deposits {
		nodes = append(nodes, zeroNode{t: q.TenorYears, zero: q.Mid()})
	}

	c := &Curve{nodes: nodes, cfg: cfg, asOf: ps.AsOf}
	for _, q := range swaps {
		lastT := c.nodes[len(c.nodes)-1].t
		if q.TenorYears < lastT {
			return nil, fmt.Errorf("curve: SWAP %q at t=%.6g is shorter than an existing node t=%.6g; overlapping swap tenors are not supported",
				q.ID, q.TenorYears, lastT)
		}
		z, err := c.bootstrapSwap(q)
		if err != nil {
			return nil, err
		}
		if q.TenorYears == lastT {
			c.nodes[len(c.nodes)-1].zero = z
		} else {
			c.nodes = append(c.nodes, zeroNode{t: q.TenorYears, zero: z})
		}
	}

	c.quotes = append(append([]marketquote.Quote(nil), deposits...), swaps...)
	if err := c.checkShape(); err != nil {
		return nil, err
	}
	if err := c.checkReprice(cfg.RepriceTol); err != nil {
		return nil, err
	}
	return c, nil
}

// fixedLegPayments builds annual payment times up to the swap maturity,
// anchored at 1 year, with a short final stub when the maturity is not an
// integer number of years.
func fixedLegPayments(maturity float64) []float64 {
	var times []float64
	for k := 1.0; k < maturity; k++ {
		times = append(times, k)
	}
	times = append(times, maturity)
	return times
}

// bootstrapSwap solves s*sum DF(Tk) + DF(Tn) = 1 for the terminal zero rate.
// A stub payment before the terminal node depends (in zero space) on the
// candidate terminal node, so the residual is solved by bisection.
func (c *Curve) bootstrapSwap(q marketquote.Quote) (float64, error) {
	maturity := q.TenorYears
	payTimes := fixedLegPayments(maturity)
	lastKnown := c.nodes[len(c.nodes)-1]

	residual := func(zTerminal float64) (float64, error) {
		sum := 0.0
		for _, t := range payTimes {
			df, err := c.dfBuilding(t, maturity, zTerminal, lastKnown)
			if err != nil {
				return 0, err
			}
			sum += df
		}
		dfTerm, err := c.dfBuilding(maturity, maturity, zTerminal, lastKnown)
		if err != nil {
			return 0, err
		}
		return q.Mid()*sum + dfTerm - 1, nil
	}

	lo, hi := -0.5, 1.0
	fLo, err := residual(lo)
	if err != nil {
		return 0, err
	}
	fHi, err := residual(hi)
	if err != nil {
		return 0, err
	}
	if fLo*fHi > 0 {
		return 0, fmt.Errorf("curve: cannot bootstrap SWAP %q (t=%.4g, rate=%.6f): residual not bracketed [%.3e, %.3e]",
			q.ID, maturity, q.Mid(), fLo, fHi)
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		fMid, err := residual(mid)
		if err != nil {
			return 0, err
		}
		if math.Abs(fMid) < 1e-14 || hi-lo < 1e-14 {
			return mid, nil
		}
		if fLo*fMid <= 0 {
			hi = mid
		} else {
			lo, fLo = mid, fMid
		}
	}
	return 0, fmt.Errorf("curve: bootstrap for SWAP %q did not converge", q.ID)
}

// dfBuilding evaluates DF during bootstrap: t beyond the last built node is
// interpolated (in zero space) between that node and the candidate terminal
// node of the swap currently being solved.
func (c *Curve) dfBuilding(t, terminalT, terminalZ float64, lastKnown zeroNode) (float64, error) {
	if t <= lastKnown.t {
		z, err := c.zeroOnBuiltNodes(t)
		if err != nil {
			return 0, err
		}
		return math.Exp(-z * t), nil
	}
	var z float64
	if c.cfg.Interpolation == FlatZero {
		z = lastKnown.zero
	} else if t == terminalT {
		z = terminalZ
	} else {
		w := (t - lastKnown.t) / (terminalT - lastKnown.t)
		z = lastKnown.zero + w*(terminalZ-lastKnown.zero)
	}
	return math.Exp(-z * t), nil
}

func (c *Curve) zeroOnBuiltNodes(t float64) (float64, error) {
	idx := sort.Search(len(c.nodes), func(i int) bool { return c.nodes[i].t >= t })
	if idx < len(c.nodes) && c.nodes[idx].t == t {
		return c.nodes[idx].zero, nil
	}
	if idx == 0 {
		return c.nodes[0].zero, nil
	}
	left := c.nodes[idx-1]
	if c.cfg.Interpolation == FlatZero || idx >= len(c.nodes) {
		return left.zero, nil
	}
	right := c.nodes[idx]
	w := (t - left.t) / (right.t - left.t)
	return left.zero + w*(right.zero-left.zero), nil
}

// checkShape enforces decreasing DFs on a dense grid covering the quoted range.
func (c *Curve) checkShape() error {
	first, last := c.nodes[1].t, c.nodes[len(c.nodes)-1].t
	steps := int(math.Ceil((last-first)*float64(c.cfg.CheckDensity))) + 1
	prevDF, prevT := 1.0, 0.0
	for i := 0; i <= steps; i++ {
		t := first + (last-first)*float64(i)/float64(steps)
		if t > last {
			t = last
		}
		z, err := c.ZeroRate(t)
		if err != nil {
			return err
		}
		df := math.Exp(-z * t)
		if df > prevDF+c.cfg.ArbTol {
			return fmt.Errorf("curve: arbitrage shape violation: DF(%.6g)=%.12f > DF(%.6g)=%.12f (discount factors must decrease)",
				t, df, prevT, prevDF)
		}
		if df <= 0 || df > 1+c.cfg.ArbTol {
			return fmt.Errorf("curve: discount factor out of (0,1] at t=%.6g: %.12f", t, df)
		}
		prevDF, prevT = df, t
	}
	return nil
}
