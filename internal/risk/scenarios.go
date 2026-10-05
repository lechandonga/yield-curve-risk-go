package risk

import (
	"fmt"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/instrument"
)

// ScenarioKind identifies a deterministic curve scenario.
type ScenarioKind string

const (
	// Parallel shifts every zero rate by Size (in decimal, up if positive).
	Parallel ScenarioKind = "PARALLEL"
	// Slope applies a linear tilt: shift(t) = Size * pivot(t), where pivot is
	// -1 at PivotShort, +1 at PivotLong (steepener for positive Size).
	Slope ScenarioKind = "SLOPE"
	// Curvature applies a butterfly: +Size at both ends, -Size at PivotMid.
	Curvature ScenarioKind = "CURVATURE"
)

// Scenario defines one revaluation scenario.
type Scenario struct {
	Kind       ScenarioKind
	Size       float64
	PivotShort float64
	PivotMid   float64
	PivotLong  float64
}

// ScenarioResult is the revaluation output for one scenario.
type ScenarioResult struct {
	Kind         ScenarioKind
	BasePV       float64
	ScenarioPV   float64
	DeltaPV      float64
	ShiftSummary string
}

func (s Scenario) validate() error {
	switch s.Kind {
	case Parallel:
		return nil
	case Slope:
		if !(s.PivotShort > 0 && s.PivotLong > s.PivotShort) {
			return fmt.Errorf("risk: slope scenario requires 0 < PivotShort < PivotLong, got %v,%v",
				s.PivotShort, s.PivotLong)
		}
	case Curvature:
		if !(s.PivotShort > 0 && s.PivotMid > s.PivotShort && s.PivotLong > s.PivotMid) {
			return fmt.Errorf("risk: curvature scenario requires 0 < PivotShort < PivotMid < PivotLong, got %v,%v,%v",
				s.PivotShort, s.PivotMid, s.PivotLong)
		}
	default:
		return fmt.Errorf("risk: unknown scenario kind %q", s.Kind)
	}
	return nil
}

func (s Scenario) shifted(c *curve.Curve) (curve.Discounter, string) {
	switch s.Kind {
	case Parallel:
		return &shiftCurve{base: c, shift: func(float64) float64 { return s.Size }},
			fmt.Sprintf("parallel shift %+.2f bp", s.Size*10000)
	case Slope:
		p := func(t float64) float64 {
			if t <= s.PivotShort {
				return -1
			}
			if t >= s.PivotLong {
				return 1
			}
			w := (t - s.PivotShort) / (s.PivotLong - s.PivotShort)
			return -1 + 2*w
		}
		return &shiftCurve{base: c, shift: func(t float64) float64 { return s.Size * p(t) }},
			fmt.Sprintf("slope tilt %+.2f bp around %.2f/%.2f", s.Size*10000, s.PivotShort, s.PivotLong)
	default: // Curvature
		q := func(t float64) float64 {
			// Linear between the three anchors, flat outside the ends (+1).
			switch {
			case t <= s.PivotShort || t >= s.PivotLong:
				return 1
			case t < s.PivotMid:
				return 1 - 2*(t-s.PivotShort)/(s.PivotMid-s.PivotShort)
			default:
				return -1 + 2*(t-s.PivotMid)/(s.PivotLong-s.PivotMid)
			}
		}
		return &shiftCurve{base: c, shift: func(t float64) float64 { return s.Size * q(t) }},
			fmt.Sprintf("curvature butterfly %+.2f bp around %.2f/%.2f/%.2f",
				s.Size*10000, s.PivotShort, s.PivotMid, s.PivotLong)
	}
}

// Revalue revalues one instrument under the scenario.
func Revalue(inst instrument.FixedCashFlows, c *curve.Curve, sc Scenario) (ScenarioResult, error) {
	if err := sc.validate(); err != nil {
		return ScenarioResult{}, err
	}
	basePV, err := inst.PV(c)
	if err != nil {
		return ScenarioResult{}, err
	}
	shifted, summary := sc.shifted(c)
	scPV, err := inst.PV(shifted)
	if err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{
		Kind:         sc.Kind,
		BasePV:       basePV,
		ScenarioPV:   scPV,
		DeltaPV:      scPV - basePV,
		ShiftSummary: summary,
	}, nil
}

// RevalueAll runs several scenarios and returns one report per scenario.
func RevalueAll(inst instrument.FixedCashFlows, c *curve.Curve, scs []Scenario) ([]ScenarioResult, error) {
	out := make([]ScenarioResult, 0, len(scs))
	for _, sc := range scs {
		res, err := Revalue(inst, c, sc)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}
