package risk

import (
	"math"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/curve"
	"github.com/lechandonga/yield-curve-risk-go/quote"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func testCurve(t *testing.T) *curve.Curve {
	t.Helper()
	mk := func(id, tenor string, r float64) quote.Quote {
		return quote.Quote{ID: id, Tenor: tenor, Bid: r - 0.0005, Ask: r + 0.0005, Timestamp: testNow}
	}
	qs := []quote.Quote{
		mk("q1", "1M", 0.018), mk("q2", "3M", 0.020), mk("q3", "6M", 0.022),
		mk("q4", "1Y", 0.025), mk("q5", "2Y", 0.028), mk("q6", "3Y", 0.030),
		mk("q7", "5Y", 0.032), mk("q8", "7Y", 0.033), mk("q9", "10Y", 0.035),
	}
	c, _, err := curve.Build(qs, curve.Config{
		QuotePolicy:   quote.Policy{MaxAge: time.Hour, ConflictTolerance: 1e-6},
		Extrapolation: curve.ExtrapolateFlatZero,
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testConfig() RevalConfig {
	return RevalConfig{
		KeyTenors: []float64{1, 2, 3, 5, 7, 10},
		// Tolerance must exceed the O(BumpSize^2) convexity residual
		// between summed separate bumps and one joint parallel bump.
		BumpSize:  1e-5,
		Tolerance: 1e-6,
	}
}

func TestBondPricingDeterministic(t *testing.T) {
	c := testCurve(t)
	b := Bond{Notional: 100, Coupon: 0.03, Maturity: 5, Frequency: 2}
	p1, err := Price(c, b)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := Price(c, b)
	if p1 != p2 {
		t.Fatalf("pricing not reproducible: %g vs %g", p1, p2)
	}
	if p1 <= 0 || p1 > 200 {
		t.Fatalf("implausible bond price %g", p1)
	}
	t.Logf("5Y 3%% semiannual bond price: %.6f (reproducible)", p1)
}

func TestKeyRateReconciliation(t *testing.T) {
	c := testCurve(t)
	b := Bond{Notional: 100, Coupon: 0.03, Maturity: 10, Frequency: 2}
	rep, err := Analyze(c, b.Cashflows(), testConfig())
	if err != nil {
		t.Fatalf("reconciliation must pass for triangular key rates: %v", err)
	}
	if len(rep.KeyRates) != 6 {
		t.Fatalf("expected 6 key rates, got %d", len(rep.KeyRates))
	}
	if rep.ParallelDelta >= 0 {
		t.Fatal("upward parallel bump must decrease bond value")
	}
	t.Logf("reconciled: sum(KRD)=%.10f parallel=%.10f gap=%.3e (tol %.0e)",
		rep.KRDSum, rep.ParallelDelta, math.Abs(rep.ReconGap), testConfig().Tolerance)
	for _, kr := range rep.KeyRates {
		t.Logf("  KRD@%gY: %.8f", kr.KeyTenor, kr.Delta)
	}
}

func TestReconciliationFailureIsExplicit(t *testing.T) {
	c := testCurve(t)
	b := Bond{Notional: 100, Coupon: 0.03, Maturity: 10, Frequency: 2}
	cfg := testConfig()
	cfg.Tolerance = 1e-15 // impossibly tight: forces an explicit failure
	rep, err := Analyze(c, b.Cashflows(), cfg)
	var re *ReconcileError
	if err == nil || !asReconcileError(err, &re) {
		t.Fatalf("expected ReconcileError, got %v", err)
	}
	if rep == nil {
		t.Fatal("report must still be returned for diagnosis")
	}
	t.Logf("tight tolerance surfaced explicit mismatch: %v", err)
}

func asReconcileError(err error, out **ReconcileError) bool {
	for err != nil {
		if re, ok := err.(*ReconcileError); ok {
			*out = re
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestScenarios(t *testing.T) {
	c := testCurve(t)
	b := Bond{Notional: 100, Coupon: 0.03, Maturity: 10, Frequency: 2}
	cfs := b.Cashflows()
	cfg := testConfig()
	base, _ := PriceCashflows(c, cfs)

	up, err := Scenario(c, cfs, cfg, ScenarioParallel, 1e-4)
	if err != nil || up >= base {
		t.Fatalf("parallel up must lower price: base=%g up=%g err=%v", base, up, err)
	}
	steep, err := Scenario(c, cfs, cfg, ScenarioSlope, 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	bfly, err := Scenario(c, cfs, cfg, ScenarioCurvature, 1e-4)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scenarios: base=%.6f parallel+1bp=%.6f steepener=%.6f butterfly=%.6f",
		base, up, steep, bfly)
}

func TestKeyRateShiftsSumToParallel(t *testing.T) {
	c := testCurve(t)
	nodes := c.Nodes()
	keys := testConfig().KeyTenors
	sum := make([]float64, len(nodes))
	for _, k := range keys {
		sh := KeyRateShifts(nodes, keys, k, 1e-4)
		for i := range sum {
			sum[i] += sh[i]
		}
	}
	for i, s := range sum {
		if math.Abs(s-1e-4) > 1e-18 {
			t.Fatalf("key-rate shifts do not sum to parallel at node %d: %g", i, s)
		}
	}
	t.Log("triangular key-rate shifts sum exactly to a parallel shift at every node")
}
