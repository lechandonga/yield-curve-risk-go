package quote

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{MaxAge: time.Hour, ConflictTolerance: 1e-6}
}

func good(id, tenor string, bid, ask float64) Quote {
	return Quote{ID: id, Tenor: tenor, Bid: bid, Ask: ask, Timestamp: testNow}
}

func TestValidateDirtyQuotes(t *testing.T) {
	pol := testPolicy()
	cases := []struct {
		name   string
		q      Quote
		reason ReasonCode
	}{
		{"missing ID", Quote{Tenor: "1Y", Bid: 0.02, Ask: 0.021, Timestamp: testNow}, ReasonMissingField},
		{"missing tenor", Quote{ID: "a", Bid: 0.02, Ask: 0.021, Timestamp: testNow}, ReasonMissingField},
		{"bad tenor format", Quote{ID: "a", Tenor: "1Q", Bid: 0.02, Ask: 0.021, Timestamp: testNow}, ReasonInvalidTenor},
		{"negative tenor", Quote{ID: "a", Tenor: "-1Y", Bid: 0.02, Ask: 0.021, Timestamp: testNow}, ReasonInvalidTenor},
		{"zero timestamp", Quote{ID: "a", Tenor: "1Y", Bid: 0.02, Ask: 0.021}, ReasonMissingField},
		{"stale quote", Quote{ID: "a", Tenor: "1Y", Bid: 0.02, Ask: 0.021, Timestamp: testNow.Add(-2 * time.Hour)}, ReasonStale},
		{"inverted market", Quote{ID: "a", Tenor: "1Y", Bid: 0.03, Ask: 0.02, Timestamp: testNow}, ReasonInvertedMarket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(tc.q, pol, testNow)
			if len(issues) == 0 {
				t.Fatalf("expected issue %s, got none", tc.reason)
			}
			found := false
			for _, i := range issues {
				if i.Reason == tc.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected reason %s, got %v", tc.reason, issues)
			}
			t.Logf("rejected as %s: %v", tc.reason, issues)
		})
	}
}

func TestDedupeDuplicatesDeterministic(t *testing.T) {
	pol := testPolicy()
	older := good("old", "1Y", 0.020, 0.020)
	older.Timestamp = testNow.Add(-time.Minute)
	newer := good("new", "1Y", 0.0200000001, 0.0200000001) // within tolerance
	out, issues := Dedupe([]Quote{older, newer}, pol)
	if len(issues) != 0 {
		t.Fatalf("duplicates within tolerance must not conflict: %v", issues)
	}
	if len(out) != 1 || out[0].ID != "new" {
		t.Fatalf("expected newest quote kept, got %+v", out)
	}
	t.Logf("duplicate resolved deterministically: kept %s", out[0].ID)
}

func TestDedupeConflictRejectsTenor(t *testing.T) {
	pol := testPolicy()
	a := good("a", "5Y", 0.030, 0.030)
	b := good("b", "5Y", 0.035, 0.035) // 50bp apart: conflict
	c := good("c", "2Y", 0.025, 0.025)
	out, issues := Dedupe([]Quote{a, b, c}, pol)
	if len(out) != 1 || out[0].ID != "c" {
		t.Fatalf("conflicting tenor must be dropped entirely, got %+v", out)
	}
	if len(issues) != 2 || issues[0].Reason != ReasonConflict {
		t.Fatalf("expected 2 CONFLICT issues, got %v", issues)
	}
	t.Logf("conflict rejected whole tenor: %v", issues)
}

func TestParseTenor(t *testing.T) {
	cases := map[string]float64{"1D": 1.0 / 365, "2W": 14.0 / 365, "6M": 0.5, "10Y": 10}
	for s, want := range cases {
		got, err := ParseTenor(s)
		if err != nil || got != want {
			t.Fatalf("ParseTenor(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	for _, bad := range []string{"", "Y", "0Y", "-3M", "1X", "abc"} {
		if _, err := ParseTenor(bad); err == nil {
			t.Fatalf("ParseTenor(%q) should fail", bad)
		}
	}
	t.Log("tenor parsing: valid forms parsed, invalid forms rejected")
}
