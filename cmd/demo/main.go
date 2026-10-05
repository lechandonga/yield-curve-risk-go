// Command demo runs the fully local yield-curve build, pricing, risk and
// online-update workflow against simulated quotes. There is no network I/O.
package main

import (
	"fmt"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/instrument"
	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
	"github.com/lechandonga/yield-curve-risk-go/internal/registry"
	"github.com/lechandonga/yield-curve-risk-go/internal/risk"
)

func localQuotes(now time.Time, level float64) []marketquote.Quote {
	return []marketquote.Quote{
		{ID: "DEP3M", Kind: marketquote.Deposit, TenorYears: 0.25, BidRate: 0.028 + level, AskRate: 0.0282 + level, QuoteTime: now.Add(-time.Minute)},
		{ID: "DEP6M", Kind: marketquote.Deposit, TenorYears: 0.5, BidRate: 0.030 + level, AskRate: 0.0302 + level, QuoteTime: now.Add(-time.Minute)},
		{ID: "DEP1Y", Kind: marketquote.Deposit, TenorYears: 1, BidRate: 0.032 + level, AskRate: 0.0322 + level, QuoteTime: now.Add(-time.Minute)},
		{ID: "SWP2Y", Kind: marketquote.Swap, TenorYears: 2, BidRate: 0.034 + level, AskRate: 0.0342 + level, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "SWP5Y", Kind: marketquote.Swap, TenorYears: 5, BidRate: 0.040 + level, AskRate: 0.0402 + level, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "SWP10Y", Kind: marketquote.Swap, TenorYears: 10, BidRate: 0.045 + level, AskRate: 0.0452 + level, QuoteTime: now.Add(-2 * time.Minute)},
	}
}

func main() {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	reg := registry.New(registry.DefaultConfig(), nil)

	v1, err := reg.Update(localQuotes(now, 0), now)
	must(err)
	fmt.Printf("已发布曲线版本 v%d（%d 条本地报价）\n", v1.Number, len(v1.Quotes))

	c := v1.Curve
	for _, t := range []float64{0.1, 0.25, 3, 7, 20} {
		z, err := c.ZeroRate(t)
		if err != nil {
			panic(err)
		}
		df := c.MustDF(t)
		fmt.Printf("  期限 %5.2fy: 零息 %.4f%%  DF %.8f\n", t, z*100, df)
	}

	bond := instrument.Bond("BOND-5Y-4%", 0.04, 1_000_000, 5)
	report, err := instrument.Price(bond, c)
	must(err)
	fmt.Printf("\n固定利率债券 %s 基准 PV = %.4f\n", bond.ID, report.PV)

	res, err := risk.Analyze(bond, c, []float64{1, 2, 5, 10}, risk.DefaultConfig())
	must(err)
	fmt.Println("关键期限敏感度（1bp，单边）:")
	for _, kd := range res.KeyDeltas {
		fmt.Printf("  关键期限 %5.2fy 桶(%.2f,%.2f]: ΔPV = %10.4f\n",
			kd.KeyTenor, kd.BucketLeft, kd.BucketRight, kd.DeltaPV)
	}
	fmt.Printf("关键期限之和 = %.6f  平行冲击 ΔPV = %.6f  对账一致\n",
		res.SumKeyDelta, res.ParallelDelta)

	scenarios := []risk.Scenario{
		{Kind: risk.Parallel, Size: 0.0001},
		{Kind: risk.Slope, Size: 0.0001, PivotShort: 1, PivotLong: 10},
		{Kind: risk.Curvature, Size: 0.0001, PivotShort: 1, PivotMid: 5, PivotLong: 10},
	}
	scResults, err := risk.RevalueAll(bond, c, scenarios)
	must(err)
	fmt.Println("情景重估:")
	for _, r := range scResults {
		fmt.Printf("  %-9s %-40s ΔPV = %10.4f\n", r.Kind, r.ShiftSummary, r.DeltaPV)
	}

	// Online update: a bad batch must not affect the published curve.
	bad := localQuotes(now, 0.001)
	bad[0].BidRate = 0.99
	if _, err := reg.Update(bad, now); err != nil {
		fmt.Printf("\n坏批次被拒绝，当前仍为 v%d: %v\n", reg.Current().Number, err)
	}
	v2, err := reg.Update(localQuotes(now.Add(time.Minute), 0.002), now.Add(time.Minute))
	must(err)
	pv2, _ := bond.PV(v2.Curve)
	fmt.Printf("好批次发布 v%d，债券 PV %.4f -> %.4f；历史 v1 仍可查询\n",
		v2.Number, report.PV, pv2)

	// Demonstrates configurable extrapolation.
	failCfg := registry.Config{Quote: marketquote.DefaultPolicy(), Curve: curve.Config{Extrapolation: curve.FailExtrap}}
	ps, _, err := marketquote.Validate(localQuotes(now, 0), now, marketquote.DefaultPolicy())
	must(err)
	strict, err := curve.Build(ps, failCfg.Curve)
	must(err)
	if _, err := strict.DF(50); err != nil {
		fmt.Printf("FAIL 外推策略下查询 50y: %v\n", err)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
