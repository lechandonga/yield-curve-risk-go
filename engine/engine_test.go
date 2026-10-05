package engine

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/curve"
	"github.com/lechandonga/yield-curve-risk-go/quote"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time { return func() time.Time { return testNow } }

func testConfig() curve.Config {
	return curve.Config{
		QuotePolicy:   quote.Policy{MaxAge: time.Hour, ConflictTolerance: 1e-6},
		Extrapolation: curve.ExtrapolateFlatZero,
	}
}

func batch(tag string, shift float64) []quote.Quote {
	tenors := []string{"1M", "3M", "6M", "1Y", "2Y", "5Y", "10Y"}
	base := []float64{0.018, 0.020, 0.022, 0.025, 0.028, 0.032, 0.035}
	var qs []quote.Quote
	for i, tn := range tenors {
		r := base[i] + shift
		qs = append(qs, quote.Quote{
			ID: fmt.Sprintf("%s-%s", tag, tn), Tenor: tn,
			Bid: r - 0.0005, Ask: r + 0.0005, Timestamp: testNow,
		})
	}
	return qs
}

func TestUpdatePublishAndHistory(t *testing.T) {
	e := New(testConfig(), fixedClock())
	v1, err := e.Update(batch("a", 0))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.Update(batch("b", 0.001))
	if err != nil {
		t.Fatal(err)
	}
	if e.Current() != v2 {
		t.Fatal("current must be the latest published version")
	}
	got, ok := e.Version(v1.ID)
	if !ok || got != v1 {
		t.Fatal("historical version must be retrievable and immutable")
	}
	// Old version's curve must be unaffected by the newer update.
	df1, _ := v1.Curve.DiscountFactor(5)
	df2, _ := v2.Curve.DiscountFactor(5)
	if df1 == df2 {
		t.Fatal("versions must carry independent curves")
	}
	t.Logf("published versions %v; v1 DF(5Y)=%.8f v2 DF(5Y)=%.8f", e.Versions(), df1, df2)
}

func TestFailedUpdateKeepsPreviousVersion(t *testing.T) {
	e := New(testConfig(), fixedClock())
	good, err := e.Update(batch("good", 0))
	if err != nil {
		t.Fatal(err)
	}
	dirty := batch("dirty", 0.002)
	dirty = append(dirty, quote.Quote{ID: "x", Tenor: "5Y", Bid: 0.09, Ask: 0.09, Timestamp: testNow}) // conflict
	if _, err := e.Update(dirty); err == nil {
		t.Fatal("dirty batch must fail")
	} else {
		t.Logf("failed update rejected with: %v", err)
	}
	if e.Current() != good {
		t.Fatal("failed update must not replace the current version")
	}
	if len(e.Versions()) != 1 {
		t.Fatalf("failed batch must not be published, versions=%v", e.Versions())
	}
	if len(e.Failures()) != 1 {
		t.Fatal("failure must be recorded")
	}
	t.Logf("rollback verified: current still version %d, failures=%d", good.ID, len(e.Failures()))
}

func TestConcurrentUpdatesReadersAndHistory(t *testing.T) {
	e := New(testConfig(), fixedClock())
	if _, err := e.Update(batch("seed", 0)); err != nil {
		t.Fatal(err)
	}

	const writers = 4
	const readers = 8
	const rounds = 50

	var wg sync.WaitGroup
	errCh := make(chan error, writers*rounds+readers*rounds)

	// Writers: half publish good batches, half publish dirty batches.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				b := batch(fmt.Sprintf("w%d-r%d", w, i), 0.0001*float64(w))
				if w%2 == 1 {
					b = append(b, quote.Quote{ID: "bad", Tenor: "5Y", Bid: 0.5, Ask: 0.5, Timestamp: testNow})
				}
				if _, err := e.Update(b); err != nil && w%2 == 0 {
					errCh <- fmt.Errorf("writer %d unexpected failure: %w", w, err)
				}
			}
		}(w)
	}

	// Readers: every observed version must be complete and self-consistent.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				v := e.Current()
				if v == nil || v.Curve == nil {
					errCh <- fmt.Errorf("reader saw nil version")
					return
				}
				if e2 := v.Curve.RepriceQuotes(); e2 > 1e-12 {
					errCh <- fmt.Errorf("reader saw inconsistent curve, reprice err %g", e2)
					return
				}
				if _, err := v.Curve.DiscountFactor(3.7); err != nil {
					errCh <- err
					return
				}
				// History lookups must never contradict the version store.
				for _, id := range e.Versions() {
					if _, ok := e.Version(id); !ok {
						errCh <- fmt.Errorf("version %d listed but not retrievable", id)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	cur := e.Current()
	t.Logf("concurrent run done: %d versions published, %d failed batches, current=v%d, readers only saw complete self-consistent curves",
		len(e.Versions()), len(e.Failures()), cur.ID)
}

func TestNoPartialPublication(t *testing.T) {
	e := New(testConfig(), fixedClock())
	if _, err := e.Update(batch("seed", 0)); err != nil {
		t.Fatal(err)
	}
	// Batch with 6 good quotes and 1 conflicting quote: must fail as a whole.
	b := batch("mix", 0.005)
	b = append(b, quote.Quote{ID: "conflict", Tenor: "2Y", Bid: 0.20, Ask: 0.20, Timestamp: testNow})
	if _, err := e.Update(b); err == nil {
		t.Fatal("partially dirty batch must fail atomically")
	}
	cur := e.Current()
	r, _ := cur.Curve.ZeroRate(2)
	if r > 0.03 {
		t.Fatalf("partial update leaked into published curve: 2Y rate=%g", r)
	}
	t.Logf("atomicity verified: mixed batch rejected wholesale, 2Y rate still %.4f", r)
}
