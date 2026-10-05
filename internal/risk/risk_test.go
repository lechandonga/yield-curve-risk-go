package risk

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/instrument"
	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

func testCurve(t *testing.T) *curve.Curve {
	t.Helper()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	quotes := []marketquote.Quote{
		{ID: "dep3m", Kind: marketquote.Deposit, TenorYears: 0.25, BidRate: 0.028, AskRate: 0.0282, QuoteTime: now.Add(-time.Minute)},
		{ID: "dep6m", Kind: marketquote.Deposit, TenorYears: 0.5, BidRate: 0.030, AskRate: 0.0302, QuoteTime: now.Add(-time.Minute)},
		{ID: "dep1y", Kind: marketquote.Deposit, TenorYears: 1, BidRate: 0.032, AskRate: 0.0322, QuoteTime: now.Add(-time.Minute)},
		{ID: "swp2y", Kind: marketquote.Swap, TenorYears: 2, BidRate: 0.034, AskRate: 0.0342, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "swp5y", Kind: marketquote.Swap, TenorYears: 5, BidRate: 0.040, AskRate: 0.0402, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "swp10y", Kind: marketquote.Swap, TenorYears: 10, BidRate: 0.045, AskRate: 0.0452, QuoteTime: now.Add(-2 * time.Minute)},
	}
	ps, _, err := marketquote.Validate(quotes, now, marketquote.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	c, err := curve.Build(ps, curve.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var testKeys = []float64{1, 2, 5, 10}

func TestKeySensitivityReconciliation(t *testing.T) {
	c := testCurve(t)
	instruments := []instrument.FixedCashFlows{
		instrument.Bond("bond5y-4pct", 0.04, 1_000_000, 5),
		instrument.Bond("bond10y-5pct", 0.05, 2_500_000, 10),
		{
			ID: "custom-flows",
			Flows: []instrument.CashFlow{
				{TimeYears: 0.7, Amount: 100},
				{TimeYears: 3.3, Amount: 200},
				{TimeYears: 9.1, Amount: 300},
			},
		},
	}

	for _, inst := range instruments {
		t.Run(inst.ID, func(t *testing.T) {
			res, err := Analyze(inst, c, testKeys, DefaultConfig())
			if err != nil {
				t.Fatalf("对账失败: %v", err)
			}
			diff := math.Abs(res.SumKeyDelta - res.ParallelDelta)
			if diff > 1e-8+1e-12*math.Abs(res.BasePV) {
				t.Fatalf("%s: 关键期限之和 %.12f 与平行变化 %.12f 差异 %.3e",
					inst.ID, res.SumKeyDelta, res.ParallelDelta, diff)
			}
			t.Logf("业务[风险对账] 用例[%s] 结果=PASS 基准PV=%.4f 平行Δ=%.6f 关键期限和=%.6f 机器误差=%.2e",
				inst.ID, res.BasePV, res.ParallelDelta, res.SumKeyDelta, diff)
		})
	}
}

func TestReconciliationFailureIsExplicit(t *testing.T) {
	c := testCurve(t)
	inst := instrument.Bond("bond5y", 0.04, 1_000_000, 5)

	// A tolerance tighter than floating-point noise must produce an explicit
	// error naming the difference rather than silently accepting it.
	cfg := Config{Bump: BPSize, ReconcileAbsTol: 0, ReconcileRelTol: 1e-18}
	_, err := Analyze(inst, c, testKeys, cfg)
	var recErr *ReconcileError
	if !errors.As(err, &recErr) {
		t.Fatalf("容差超限时应返回 *ReconcileError，实际: %v", err)
	}
	if recErr.Difference == 0 {
		t.Fatalf("对账错误必须指出实际差异量")
	}
	t.Logf("业务[对账失败显式报错] 用例[容差1e-18触发ReconcileError] 结果=PASS 差异=%.3e 容差=%.0e",
		recErr.Difference, recErr.RelTolerance)
}

func TestScenariosParallelSlopeCurvature(t *testing.T) {
	c := testCurve(t)
	inst := instrument.Bond("bond10y", 0.045, 1_000_000, 10)

	scenarios := []Scenario{
		{Kind: Parallel, Size: BPSize},
		{Kind: Slope, Size: BPSize, PivotShort: 1, PivotLong: 10},
		{Kind: Curvature, Size: BPSize, PivotShort: 1, PivotMid: 5, PivotLong: 10},
		{Kind: Parallel, Size: -BPSize},
	}
	results, err := RevalueAll(inst, c, scenarios)
	if err != nil {
		t.Fatal(err)
	}

	// Parallel +1bp must move a positive-cash-flow bond's PV down; -1bp up.
	if results[0].DeltaPV >= 0 || results[3].DeltaPV <= 0 {
		t.Fatalf("平行冲击方向异常: +1bp Δ=%.6f, -1bp Δ=%.6f", results[0].DeltaPV, results[3].DeltaPV)
	}
	// Up/down parallel shifts must be near-symmetric (second-order small).
	asym := math.Abs(results[0].DeltaPV + results[3].DeltaPV)
	// Convexity makes the +1bp/-1bp deltas differ by a second-order amount;
	// it must be small relative to the delta magnitude (here below 0.1%).
	if asym/math.Abs(results[0].DeltaPV) > 1e-3 {
		t.Fatalf("上下平行冲击凸性不对称度异常: asym=%.3e", asym)
	}
	for _, r := range results {
		t.Logf("业务[情景重估] 用例[%s] 结果=PASS %s 基准PV=%.4f 情景PV=%.4f Δ=%.6f",
			r.Kind, r.ShiftSummary, r.BasePV, r.ScenarioPV, r.DeltaPV)
	}
}

func TestScenarioValidation(t *testing.T) {
	c := testCurve(t)
	inst := instrument.Bond("bond10y", 0.045, 1_000_000, 10)
	bad := []Scenario{
		{Kind: ScenarioKind("TWIST"), Size: BPSize},
		{Kind: Slope, Size: BPSize, PivotShort: 10, PivotLong: 1},
		{Kind: Curvature, Size: BPSize, PivotShort: 5, PivotMid: 1, PivotLong: 10},
	}
	for i, sc := range bad {
		if _, err := Revalue(inst, c, sc); err == nil {
			t.Fatalf("非法情景 %d 应报错", i)
		}
	}
	t.Logf("业务[情景参数校验] 用例[未知类型/锚点顺序错误] 结果=PASS")
}

func TestRiskReproducibility(t *testing.T) {
	c := testCurve(t)
	inst := instrument.Bond("bond10y", 0.045, 1_000_000, 10)
	r1, err := Analyze(inst, c, testKeys, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Analyze(inst, c, testKeys, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if r1.BasePV != r2.BasePV || r1.ParallelDelta != r2.ParallelDelta {
		t.Fatalf("同快照同配置结果不稳定")
	}
	for i := range r1.KeyDeltas {
		if r1.KeyDeltas[i] != r2.KeyDeltas[i] {
			t.Fatalf("关键期限 %v 敏感度不稳定", r1.KeyDeltas[i].KeyTenor)
		}
	}
	t.Logf("业务[可复现] 用例[重复风险计算逐位一致] 结果=PASS")
}
