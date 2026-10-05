// Package risk computes key-tenor sensitivities and standard rate scenarios
// for fixed-cash-flow instruments, and enforces an explicit reconciliation
// between summed key sensitivities and a parallel-shock revaluation.
//
// Bump convention (the reconciliation identity):
//   - Shifts are applied to continuously compounded ZERO rates, in absolute
//     decimal units (1bp = 0.0001), one-sided (up) bumps.
//   - A key-tenor bump is a tent-free piecewise-flat shift: the shift equals h
//     on the half-open Voronoi bucket around the key tenor
//     (midLeft, midRight], with flat extrapolation of the shift beyond the
//     first/last key. The buckets partition the whole tenor axis, so the sum
//     of one key bump at every key is exactly one uniform parallel bump.
//   - Consequently sum_k KT_k == dPV(parallel) up to floating-point error;
//     any gap larger than the configurable reconciliation tolerance is reported
//     as *ReconcileError naming
//     the offending instrument and the magnitude of the difference.
package risk

import (
	"fmt"
	"math"
	"sort"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/instrument"
)

// BPSize is one basis point in decimal rate units.
const BPSize = 0.0001

// Config controls risk calculations.
type Config struct {
	// Bump is the absolute zero-rate bump size (default 1bp).
	Bump float64
	// ReconcileAbsTol is the absolute PV tolerance for key-vs-parallel
	// reconciliation (default 1e-8).
	ReconcileAbsTol float64
	// ReconcileRelTol is the relative tolerance against |base PV|
	// (default 1e-10). The effective tolerance is
	// ReconcileAbsTol + ReconcileRelTol*|BasePV|.
	ReconcileRelTol float64
}

// DefaultConfig returns the standard risk configuration.
func DefaultConfig() Config {
	return Config{Bump: BPSize, ReconcileAbsTol: 1e-8, ReconcileRelTol: 1e-10}
}

func (c Config) withDefaults() Config {
	if c.Bump <= 0 {
		c.Bump = BPSize
	}
	if c.ReconcileAbsTol < 0 {
		c.ReconcileAbsTol = 1e-8
	}
	if c.ReconcileRelTol < 0 {
		c.ReconcileRelTol = 1e-10
	}
	return c
}

// shiftCurve overlays an arbitrary zero-rate shift function on a base curve.
type shiftCurve struct {
	base  *curve.Curve
	shift func(t float64) float64
}

func (s *shiftCurve) DF(t float64) (float64, error) {
	baseDF, err := s.base.DF(t)
	if err != nil {
		return 0, err
	}
	return baseDF * math.Exp(-s.shift(t)*t), nil
}

// parallelShift returns a curve with every zero rate shifted by h.
func parallelShift(c *curve.Curve, h float64) curve.Discounter {
	return &shiftCurve{base: c, shift: func(float64) float64 { return h }}
}

// keyShift returns a piecewise-flat tent-free bump on (midLeft, midRight].
func keyShift(c *curve.Curve, keys []float64, idx int, h float64) curve.Discounter {
	midLeft := 0.0
	if idx > 0 {
		midLeft = (keys[idx-1] + keys[idx]) / 2
	}
	midRight := math.Inf(1)
	if idx < len(keys)-1 {
		midRight = (keys[idx] + keys[idx+1]) / 2
	}
	return &shiftCurve{
		base: c,
		shift: func(t float64) float64 {
			if t > midLeft && t <= midRight {
				return h
			}
			return 0
		},
	}
}

// KeySensitivity is the one-sided PV change for one key-tenor bucket.
type KeySensitivity struct {
	KeyTenor    float64
	BucketLeft  float64
	BucketRight float64
	DeltaPV     float64
}

// ReconcileError states that key sensitivities do not sum to the parallel move.
type ReconcileError struct {
	InstrumentID  string
	ParallelDelta float64
	SumKeyDelta   float64
	Difference    float64
	AbsTolerance  float64
	RelTolerance  float64
	BasePV        float64
}

func (e *ReconcileError) Error() string {
	return fmt.Sprintf("risk: reconciliation failure for %q: parallel delta %.12f vs sum of key deltas %.12f, difference %.3e exceeds tolerance %.3e (abs %.3e + rel %.3e x |PV| %.6f)",
		e.InstrumentID, e.ParallelDelta, e.SumKeyDelta, e.Difference,
		e.AbsTolerance+e.RelTolerance*math.Abs(e.BasePV), e.AbsTolerance, e.RelTolerance, e.BasePV)
}

// Result contains the complete, reproducible risk output for one instrument.
type Result struct {
	InstrumentID  string
	BasePV        float64
	ParallelUpPV  float64
	ParallelDelta float64
	KeyDeltas     []KeySensitivity
	SumKeyDelta   float64
}

// Analyze computes base PV, key-tenor sensitivities at the given key tenors
// (sorted, positive, deduplicated), and reconciles them against a parallel
// one-sided bump of the same size.
func Analyze(inst instrument.FixedCashFlows, c *curve.Curve, keyTenors []float64, cfg Config) (*Result, error) {
	cfg = cfg.withDefaults()
	keys := normalizeKeys(keyTenors)
	if len(keys) == 0 {
		return nil, fmt.Errorf("risk: at least one key tenor is required")
	}

	basePV, err := inst.PV(c)
	if err != nil {
		return nil, err
	}
	parallelPV, err := inst.PV(parallelShift(c, cfg.Bump))
	if err != nil {
		return nil, err
	}

	res := &Result{
		InstrumentID:  inst.ID,
		BasePV:        basePV,
		ParallelUpPV:  parallelPV,
		ParallelDelta: parallelPV - basePV,
		KeyDeltas:     make([]KeySensitivity, 0, len(keys)),
	}
	for i, k := range keys {
		bumpedPV, err := inst.PV(keyShift(c, keys, i, cfg.Bump))
		if err != nil {
			return nil, err
		}
		left := 0.0
		if i > 0 {
			left = (keys[i-1] + k) / 2
		}
		right := math.Inf(1)
		if i < len(keys)-1 {
			right = (k + keys[i+1]) / 2
		}
		kd := KeySensitivity{
			KeyTenor: k, BucketLeft: left, BucketRight: right,
			DeltaPV: bumpedPV - basePV,
		}
		res.KeyDeltas = append(res.KeyDeltas, kd)
		res.SumKeyDelta += kd.DeltaPV
	}

	tol := cfg.ReconcileAbsTol + cfg.ReconcileRelTol*math.Abs(basePV)
	if diff := math.Abs(res.SumKeyDelta - res.ParallelDelta); diff > tol {
		return nil, &ReconcileError{
			InstrumentID:  inst.ID,
			ParallelDelta: res.ParallelDelta,
			SumKeyDelta:   res.SumKeyDelta,
			Difference:    res.SumKeyDelta - res.ParallelDelta,
			AbsTolerance:  cfg.ReconcileAbsTol,
			RelTolerance:  cfg.ReconcileRelTol,
			BasePV:        basePV,
		}
	}
	return res, nil
}

func normalizeKeys(in []float64) []float64 {
	keys := make([]float64, 0, len(in))
	for _, k := range in {
		if math.IsNaN(k) || math.IsInf(k, 0) || k <= 0 {
			continue
		}
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	out := keys[:0]
	for _, k := range keys {
		if len(out) == 0 || k != out[len(out)-1] {
			out = append(out, k)
		}
	}
	return out
}
