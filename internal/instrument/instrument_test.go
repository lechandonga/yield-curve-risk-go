package instrument

import (
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

func flatCurve(t *testing.T) *curve.Curve {
	t.Helper()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	quotes := []marketquote.Quote{
		{ID: "dep1y", Kind: marketquote.Deposit, TenorYears: 1, BidRate: 0.03, AskRate: 0.03, QuoteTime: now},
		{ID: "swp10y", Kind: marketquote.Swap, TenorYears: 10, BidRate: 0.03, AskRate: 0.03, QuoteTime: now},
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

func TestFixedCashFlowPricing(t *testing.T) {
	c := flatCurve(t)

	t.Run("1年期零息债券精确折现", func(t *testing.T) {
		zcb := FixedCashFlows{ID: "zcb1", Flows: []CashFlow{{TimeYears: 1, Amount: 100}}}
		pv, err := zcb.PV(c)
		if err != nil {
			t.Fatal(err)
		}
		want := 100 * c.MustDF(1)
		if pv != want {
			t.Fatalf("PV %.10f != %.10f", pv, want)
		}
		t.Logf("业务[固定现金流定价] 用例[单笔零息] 结果=PASS PV=%.6f", pv)
	})

	t.Run("平价债券在3%平坦曲线上约等于面值", func(t *testing.T) {
		par := Bond("par10", 0.03, 1_000_000, 10)
		rep, err := Price(par, c)
		if err != nil {
			t.Fatal(err)
		}
		if diff := rep.PV - 1_000_000; diff > 1e-5 || diff < -1e-5 {
			t.Fatalf("平价债券 PV 偏离面值: %.10f 偏差 %.3e", rep.PV, diff)
		}
		if len(rep.FlowPV) != 10 {
			t.Fatalf("10年期年付息债券应有10笔流，实际 %d", len(rep.FlowPV))
		}
		t.Logf("业务[固定现金流定价] 用例[10年3%%平价债券] 结果=PASS PV=%.6f 流数=%d", rep.PV, len(rep.FlowPV))
	})
}

func TestInvalidInstruments(t *testing.T) {
	c := flatCurve(t)
	bad := []FixedCashFlows{
		{ID: "", Flows: []CashFlow{{TimeYears: 1, Amount: 1}}},
		{ID: "empty"},
		{ID: "badtime", Flows: []CashFlow{{TimeYears: 0, Amount: 1}}},
		{ID: "badamount", Flows: []CashFlow{{TimeYears: 1, Amount: 0}}},
	}
	for i, inst := range bad {
		if _, err := inst.PV(c); err == nil {
			t.Fatalf("非法工具 %d 必须报错", i)
		}
	}
	t.Logf("业务[工具校验] 用例[空ID/无流/非法期限/零金额] 结果=PASS")
}
