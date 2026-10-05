package curve

import (
	"math"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/quote"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func sampleQuotes() []quote.Quote {
	mk := func(id, tenor string, r float64) quote.Quote {
		return quote.Quote{ID: id, Tenor: tenor, Bid: r - 0.0005, Ask: r + 0.0005, Timestamp: testNow}
	}
	return []quote.Quote{
		mk("d1", "1M", 0.018),
		mk("d2", "3M", 0.020),
		mk("d3", "6M", 0.022),
		mk("d4", "1Y", 0.025),
		mk("d5", "2Y", 0.028),
		mk("d6", "5Y", 0.032),
		mk("d7", "10Y", 0.035),
	}
}

func defaultConfig() Config {
	return Config{
		QuotePolicy:   quote.Policy{MaxAge: time.Hour, ConflictTolerance: 1e-6},
		Extrapolation: ExtrapolateFlatZero,
	}
}

func TestBuildAndSelfConsistentRepricing(t *testing.T) {
	c, issues, err := Build(sampleQuotes(), defaultConfig(), testNow)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if e := c.RepriceQuotes(); e > 1e-12 {
		t.Fatalf("curve does not reprice its own quotes, max err %g", e)
	}
	t.Logf("self-consistent repricing: max error %.3e over %d quotes", c.RepriceQuotes(), len(c.Quotes()))
}

func TestArbitraryTenorQueryAndMonotoneDF(t *testing.T) {
	c, _, err := Build(sampleQuotes(), defaultConfig(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	prev := 1.0
	for tm := 0.01; tm <= 10.0; tm += 0.05 {
		df, err := c.DiscountFactor(tm)
		if err != nil {
			t.Fatal(err)
		}
		if df >= prev {
			t.Fatalf("DF not strictly decreasing at t=%g: %g >= %g (arbitrage)", tm, df, prev)
		}
		prev = df
	}
	t.Log("DF strictly decreasing over [0.01, 10]: no arbitrage shape inside quoted range")
}

func TestExtrapolationModes(t *testing.T) {
	flat, _, err := Build(sampleQuotes(), defaultConfig(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	r20a, err1 := flat.ZeroRate(20)
	r20b, err2 := flat.ZeroRate(25)
	if err1 != nil || err2 != nil || r20a != r20b {
		t.Fatalf("flat extrapolation must be stable: %g %g (%v %v)", r20a, r20b, err1, err2)
	}
	t.Logf("flat extrapolation: r(20Y)=r(25Y)=%.4f", r20a)

	cfg := defaultConfig()
	cfg.Extrapolation = ExtrapolateError
	strict, _, err := Build(sampleQuotes(), cfg, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.DiscountFactor(20); err == nil {
		t.Fatal("expected out-of-range error with ExtrapolateError")
	}
	t.Log("strict extrapolation: out-of-range query rejected explicitly")
}

func TestDirtyBatchFailsDeterministically(t *testing.T) {
	qs := sampleQuotes()
	qs = append(qs,
		quote.Quote{ID: "bad1", Tenor: "1Y", Bid: 0.05, Ask: 0.04, Timestamp: testNow},     // inverted
		quote.Quote{ID: "bad2", Tenor: "junk", Bid: 0.03, Ask: 0.03, Timestamp: testNow},   // bad tenor
		quote.Quote{ID: "bad3", Tenor: "7Y", Bid: 0.03, Ask: 0.03, Timestamp: time.Time{}}, // missing ts
		quote.Quote{ID: "c1", Tenor: "5Y", Bid: 0.09, Ask: 0.09, Timestamp: testNow},       // conflicts with d6
	)
	_, issues, err := Build(qs, defaultConfig(), testNow)
	if err == nil {
		t.Fatal("dirty batch must fail the build")
	}
	be, ok := err.(*BuildError)
	if !ok {
		t.Fatalf("expected BuildError, got %T", err)
	}
	reasons := map[quote.ReasonCode]bool{}
	for _, i := range be.Issues {
		reasons[i.Reason] = true
	}
	for _, want := range []quote.ReasonCode{quote.ReasonInvertedMarket, quote.ReasonInvalidTenor, quote.ReasonMissingField, quote.ReasonConflict} {
		if !reasons[want] {
			t.Fatalf("missing reason %s in %v", want, be.Issues)
		}
	}
	t.Logf("dirty batch rejected with %d distinguishable issues: %v", len(issues), err)
}

func TestArbitrageShapeRejected(t *testing.T) {
	// Zero rates falling fast enough to imply a negative forward.
	qs := []quote.Quote{
		{ID: "a", Tenor: "1Y", Bid: 0.10, Ask: 0.10, Timestamp: testNow},
		{ID: "b", Tenor: "2Y", Bid: 0.01, Ask: 0.01, Timestamp: testNow},
	}
	_, _, err := Build(qs, defaultConfig(), testNow)
	be, ok := err.(*BuildError)
	if !ok || len(be.Structural) == 0 {
		t.Fatalf("expected structural arbitrage error, got %v", err)
	}
	t.Logf("arbitrage shape rejected: %v", be.Structural)
}

func TestReproducibility(t *testing.T) {
	c1, _, _ := Build(sampleQuotes(), defaultConfig(), testNow)
	c2, _, _ := Build(sampleQuotes(), defaultConfig(), testNow)
	for _, tt := range []float64{0.3, 1.5, 7.7, 30} {
		d1, _ := c1.DiscountFactor(tt)
		d2, _ := c2.DiscountFactor(tt)
		if d1 != d2 {
			t.Fatalf("non-reproducible DF at %g: %g vs %g", tt, d1, d2)
		}
		if math.IsNaN(d1) {
			t.Fatal("NaN discount factor")
		}
	}
	t.Log("identical inputs produce bit-identical discount factors")
}
