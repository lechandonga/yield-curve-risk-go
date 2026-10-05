// Package risk prices fixed-cashflow instruments and computes
// key-rate sensitivities and scenario revaluations off a curve.
//
// Reconciliation contract: the sum of key-rate sensitivities must
// match the revaluation change under a parallel shock within a
// configurable tolerance. If it does not, Reconcile returns an
// explicit ReconcileError identifying both sides and the gap —
// never a silently approximated number.
package risk

import (
	"fmt"
	"math"

	"github.com/lechandonga/yield-curve-risk-go/curve"
)

// Cashflow is a fixed payment at time t (years from valuation).
type Cashflow struct {
	T      float64
	Amount float64
}

// Bond is a fixed-rate bullet bond.
type Bond struct {
	Notional  float64
	Coupon    float64 // annual coupon rate on notional
	Maturity  float64 // years
	Frequency int     // payments per year (1 or 2)
}

// Cashflows generates the deterministic cashflow schedule.
func (b Bond) Cashflows() []Cashflow {
	if b.Frequency <= 0 {
		b.Frequency = 1
	}
	step := 1 / float64(b.Frequency)
	var cfs []Cashflow
	n := int(math.Round(b.Maturity * float64(b.Frequency)))
	for i := 1; i <= n; i++ {
		t := float64(i) * step
		if t > b.Maturity+1e-12 {
			break
		}
		amt := b.Notional * b.Coupon / float64(b.Frequency)
		if i == n {
			amt += b.Notional
		}
		cfs = append(cfs, Cashflow{T: t, Amount: amt})
	}
	return cfs
}

// PriceCashflows discounts fixed cashflows on the curve.
func PriceCashflows(c *curve.Curve, cfs []Cashflow) (float64, error) {
	pv := 0.0
	for _, cf := range cfs {
		df, err := c.DiscountFactor(cf.T)
		if err != nil {
			return 0, fmt.Errorf("pricing cashflow at t=%g: %w", cf.T, err)
		}
		pv += cf.Amount * df
	}
	return pv, nil
}

// Price prices a bond on the curve.
func Price(c *curve.Curve, b Bond) (float64, error) {
	return PriceCashflows(c, b.Cashflows())
}

// KeyRateShifts returns triangular zero-rate shifts (in rate units)
// for a bump of size bump at key tenor keyT, given the curve nodes.
// The shift is 1 at the key node and falls linearly to 0 at the
// adjacent key tenors. Below the lowest (above the highest) key tenor
// the shift stays at the full bump of the edge key, so that the
// key-rate shifts sum exactly to a parallel shift at every node.
func KeyRateShifts(nodes []curve.Node, keyTenors []float64, keyT, bump float64) []float64 {
	// Find bracketing key tenors.
	lo, hi := math.Inf(-1), math.Inf(1)
	for _, k := range keyTenors {
		if k < keyT && k > lo {
			lo = k
		}
		if k > keyT && k < hi {
			hi = k
		}
	}
	firstKey, lastKey := keyTenors[0], keyTenors[len(keyTenors)-1]
	shifts := make([]float64, len(nodes))
	for i, n := range nodes {
		t := n.Tenor
		switch {
		case t <= firstKey:
			if keyT == firstKey {
				shifts[i] = bump
			}
		case t >= lastKey:
			if keyT == lastKey {
				shifts[i] = bump
			}
		case t == keyT:
			shifts[i] = bump
		case t > lo && t < keyT:
			if !math.IsInf(lo, -1) {
				shifts[i] = bump * (t - lo) / (keyT - lo)
			}
		case t > keyT && t < hi:
			if !math.IsInf(hi, 1) {
				shifts[i] = bump * (hi - t) / (hi - keyT)
			}
		}
	}
	return shifts
}

// KeyRateSensitivity is the value change from a 1-unit bump at one key tenor.
type KeyRateSensitivity struct {
	KeyTenor float64
	Delta    float64 // value change for BumpSize bump
}

// ScenarioKind selects the shape of a curve scenario.
type ScenarioKind int

const (
	ScenarioParallel  ScenarioKind = iota // same shift at every node
	ScenarioSlope                         // linear in tenor, pivoted at the first key tenor
	ScenarioCurvature                     // quadratic butterfly around the middle key tenor
)

// ScenarioShifts builds per-node shifts for a scenario of total size.
func ScenarioShifts(nodes []curve.Node, keyTenors []float64, kind ScenarioKind, size float64) []float64 {
	shifts := make([]float64, len(nodes))
	if len(keyTenors) == 0 {
		for i := range shifts {
			shifts[i] = size
		}
		return shifts
	}
	t0 := keyTenors[0]
	tEnd := keyTenors[len(keyTenors)-1]
	tMid := (t0 + tEnd) / 2
	half := (tEnd - t0) / 2
	for i, n := range nodes {
		switch kind {
		case ScenarioParallel:
			shifts[i] = size
		case ScenarioSlope:
			// -size at short end, +size at long end, 0 at t0.
			if tEnd == t0 {
				shifts[i] = 0
			} else {
				shifts[i] = size * (2*(n.Tenor-t0)/(tEnd-t0) - 1)
			}
		case ScenarioCurvature:
			// +size at wings, -size at the middle (butterfly).
			if half == 0 {
				shifts[i] = 0
			} else {
				x := (n.Tenor - tMid) / half
				shifts[i] = size * (x*x - 0.5) * 2 // +size at wings, -size at center
			}
		}
	}
	return shifts
}

// RevalConfig controls bump sizes and reconciliation tolerance.
type RevalConfig struct {
	KeyTenors []float64 // key tenors for KRD, e.g. [1, 2, 5, 10]
	BumpSize  float64   // rate bump, e.g. 1e-4 for 1bp
	Tolerance float64   // max |sum(KRD deltas) - parallel delta| allowed
}

// Report holds a full pricing + risk + reconciliation result.
type Report struct {
	Price         float64
	KeyRates      []KeyRateSensitivity
	ParallelDelta float64 // value change under parallel bump of BumpSize
	KRDSum        float64 // sum of key-rate deltas
	ReconGap      float64 // KRDSum - ParallelDelta
}

// ReconcileError is returned when KRD sum and parallel revaluation
// disagree beyond tolerance. Both sides and the gap are exposed.
type ReconcileError struct {
	KRDSum        float64
	ParallelDelta float64
	Gap           float64
	Tolerance     float64
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("risk reconciliation failed: sum(KRD)=%.10f vs parallel=%.10f, gap=%.3e exceeds tolerance %.3e",
		e.KRDSum, e.ParallelDelta, e.Gap, e.Tolerance)
}

// Analyze prices the cashflows and computes key-rate sensitivities and
// a parallel revaluation, then reconciles them. Deterministic for a
// given curve and config.
func Analyze(c *curve.Curve, cfs []Cashflow, cfg RevalConfig) (*Report, error) {
	base, err := PriceCashflows(c, cfs)
	if err != nil {
		return nil, err
	}
	nodes := c.Nodes()

	rep := &Report{Price: base}

	// Parallel scenario.
	parShifts := ScenarioShifts(nodes, cfg.KeyTenors, ScenarioParallel, cfg.BumpSize)
	parCurve, err := c.Shifted(parShifts)
	if err != nil {
		return nil, err
	}
	parPrice, err := PriceCashflows(parCurve, cfs)
	if err != nil {
		return nil, err
	}
	rep.ParallelDelta = parPrice - base

	// Key-rate sensitivities.
	for _, k := range cfg.KeyTenors {
		shifts := KeyRateShifts(nodes, cfg.KeyTenors, k, cfg.BumpSize)
		kc, err := c.Shifted(shifts)
		if err != nil {
			return nil, err
		}
		p, err := PriceCashflows(kc, cfs)
		if err != nil {
			return nil, err
		}
		d := p - base
		rep.KeyRates = append(rep.KeyRates, KeyRateSensitivity{KeyTenor: k, Delta: d})
		rep.KRDSum += d
	}

	rep.ReconGap = rep.KRDSum - rep.ParallelDelta
	if math.Abs(rep.ReconGap) > cfg.Tolerance {
		return rep, &ReconcileError{
			KRDSum:        rep.KRDSum,
			ParallelDelta: rep.ParallelDelta,
			Gap:           rep.ReconGap,
			Tolerance:     cfg.Tolerance,
		}
	}
	return rep, nil
}

// Scenario reprices cashflows under a named scenario.
func Scenario(c *curve.Curve, cfs []Cashflow, cfg RevalConfig, kind ScenarioKind, size float64) (float64, error) {
	nodes := c.Nodes()
	sc, err := c.Shifted(ScenarioShifts(nodes, cfg.KeyTenors, kind, size))
	if err != nil {
		return 0, err
	}
	return PriceCashflows(sc, cfs)
}
