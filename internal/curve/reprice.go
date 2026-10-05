package curve

import (
	"fmt"

	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

// ParSwapRate returns the annual-fixed par rate implied by the curve for the
// given maturity: s = (1 - DF(Tn)) / sum_k DF(Tk).
func ParSwapRate(d Discounter, maturity float64) (float64, error) {
	times := fixedLegPayments(maturity)
	annuity := 0.0
	for _, t := range times {
		df, err := d.DF(t)
		if err != nil {
			return 0, err
		}
		annuity += df
	}
	dfTerm, err := d.DF(maturity)
	if err != nil {
		return 0, err
	}
	if annuity == 0 {
		return 0, fmt.Errorf("curve: zero annuity for maturity %.6g", maturity)
	}
	return (1 - dfTerm) / annuity, nil
}

// RepriceFailure ties a quote that fails self-consistency to its residual.
type RepriceFailure struct {
	QuoteID  string
	Kind     marketquote.InstrumentKind
	Tenor    float64
	Expected float64
	Actual   float64
	Residual float64
	Tol      float64
}

func (e *RepriceFailure) Error() string {
	if e.Kind == marketquote.Deposit {
		return fmt.Sprintf("curve: self-reprice failure for deposit %q t=%.6g: quoted zero %.12f vs curve zero %.12f, residual %.3e > tol %.3e",
			e.QuoteID, e.Tenor, e.Expected, e.Actual, e.Residual, e.Tol)
	}
	return fmt.Sprintf("curve: self-reprice failure for swap %q t=%.6g: quoted par %.12f vs curve par %.12f, residual %.3e > tol %.3e",
		e.QuoteID, e.Tenor, e.Expected, e.Actual, e.Residual, e.Tol)
}

// checkReprice re-prices every input quote off the finished curve.
func (c *Curve) checkReprice(tol float64) error {
	for _, q := range c.quotes {
		switch q.Kind {
		case marketquote.Deposit:
			z, err := c.ZeroRate(q.TenorYears)
			if err != nil {
				return err
			}
			if res := z - q.Mid(); abs(res) > tol {
				return &RepriceFailure{
					QuoteID: q.ID, Kind: q.Kind, Tenor: q.TenorYears,
					Expected: q.Mid(), Actual: z, Residual: res, Tol: tol,
				}
			}
		case marketquote.Swap:
			implied, err := ParSwapRate(c, q.TenorYears)
			if err != nil {
				return err
			}
			if res := implied - q.Mid(); abs(res) > tol {
				return &RepriceFailure{
					QuoteID: q.ID, Kind: q.Kind, Tenor: q.TenorYears,
					Expected: q.Mid(), Actual: implied, Residual: res, Tol: tol,
				}
			}
		}
	}
	return nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
