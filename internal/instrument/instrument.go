// Package instrument prices fixed-cash-flow interest-rate instruments off a
// curve.Discounter. All inputs are local; pricing is a pure function of
// (instrument, curve) and therefore deterministic and reproducible.
package instrument

import (
	"fmt"
	"math"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
)

// CashFlow is one fixed payment at time TimeYears.
type CashFlow struct {
	TimeYears float64
	Amount    float64
}

// FixedCashFlows is a generic fixed-payment instrument (e.g. a fixed-rate
// bond, a loan or the fixed leg of a swap represented as explicit flows).
type FixedCashFlows struct {
	ID    string
	Flows []CashFlow
}

// Validate rejects missing ids, empty schedules and bad times/amounts.
func (f FixedCashFlows) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("instrument: fixed cash-flow instrument id is empty")
	}
	if len(f.Flows) == 0 {
		return fmt.Errorf("instrument %q: no cash flows", f.ID)
	}
	for i, cf := range f.Flows {
		if math.IsNaN(cf.TimeYears) || math.IsInf(cf.TimeYears, 0) || cf.TimeYears <= 0 {
			return fmt.Errorf("instrument %q: flow %d has invalid payment time %v", f.ID, i, cf.TimeYears)
		}
		if math.IsNaN(cf.Amount) || math.IsInf(cf.Amount, 0) || cf.Amount == 0 {
			return fmt.Errorf("instrument %q: flow %d at t=%.6g has invalid amount %v", f.ID, i, cf.TimeYears, cf.Amount)
		}
	}
	return nil
}

// PV returns present value as sum of Amount*DF(t).
func (f FixedCashFlows) PV(d curve.Discounter) (float64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	pv := 0.0
	for _, cf := range f.Flows {
		df, err := d.DF(cf.TimeYears)
		if err != nil {
			return 0, fmt.Errorf("instrument %q: discount flow at t=%.6g: %w", f.ID, cf.TimeYears, err)
		}
		pv += cf.Amount * df
	}
	return pv, nil
}

// PriceReport holds the PV and the flow-level decomposition (for explainability).
type PriceReport struct {
	InstrumentID string
	PV           float64
	FlowPV       []FlowPV
}

// FlowPV is the discounted value of one cash flow.
type FlowPV struct {
	TimeYears float64
	Amount    float64
	DF        float64
	PV        float64
}

// Price returns the full explainable price report.
func Price(f FixedCashFlows, d curve.Discounter) (PriceReport, error) {
	if err := f.Validate(); err != nil {
		return PriceReport{}, err
	}
	rep := PriceReport{InstrumentID: f.ID, FlowPV: make([]FlowPV, 0, len(f.Flows))}
	for _, cf := range f.Flows {
		df, err := d.DF(cf.TimeYears)
		if err != nil {
			return PriceReport{}, fmt.Errorf("instrument %q: discount flow at t=%.6g: %w", f.ID, cf.TimeYears, err)
		}
		rep.FlowPV = append(rep.FlowPV, FlowPV{
			TimeYears: cf.TimeYears, Amount: cf.Amount, DF: df, PV: cf.Amount * df,
		})
		rep.PV += cf.Amount * df
	}
	return rep, nil
}

// Bond builds annual fixed-coupon flows ending with redemption at maturity.
func Bond(id string, couponRate, faceValue, maturityYears float64) FixedCashFlows {
	var flows []CashFlow
	for t := 1.0; t < maturityYears; t++ {
		flows = append(flows, CashFlow{TimeYears: t, Amount: faceValue * couponRate})
	}
	flows = append(flows, CashFlow{TimeYears: maturityYears, Amount: faceValue * (1 + couponRate)})
	return FixedCashFlows{ID: id, Flows: flows}
}
