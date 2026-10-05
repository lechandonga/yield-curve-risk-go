package curve

import (
	"math"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
)

func testQuotes(now time.Time) []marketquote.Quote {
	return []marketquote.Quote{
		{ID: "dep3m", Kind: marketquote.Deposit, TenorYears: 0.25, BidRate: 0.028, AskRate: 0.0282, QuoteTime: now.Add(-time.Minute)},
		{ID: "dep6m", Kind: marketquote.Deposit, TenorYears: 0.5, BidRate: 0.030, AskRate: 0.0302, QuoteTime: now.Add(-time.Minute)},
		{ID: "dep1y", Kind: marketquote.Deposit, TenorYears: 1, BidRate: 0.032, AskRate: 0.0322, QuoteTime: now.Add(-time.Minute)},
		{ID: "swp2y", Kind: marketquote.Swap, TenorYears: 2, BidRate: 0.034, AskRate: 0.0342, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "swp3y", Kind: marketquote.Swap, TenorYears: 3, BidRate: 0.036, AskRate: 0.0362, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "swp5y", Kind: marketquote.Swap, TenorYears: 5, BidRate: 0.040, AskRate: 0.0402, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: "swp10y", Kind: marketquote.Swap, TenorYears: 10, BidRate: 0.045, AskRate: 0.0452, QuoteTime: now.Add(-2 * time.Minute)},
	}
}

func buildTestCurve(t *testing.T, cfg Config) *Curve {
	t.Helper()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ps, _, err := marketquote.Validate(testQuotes(now), now, marketquote.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	c, err := Build(ps, cfg)
	if err != nil {
		t.Fatalf("曲线构建失败: %v", err)
	}
	return c
}

func TestBootstrapAndSelfReprice(t *testing.T) {
	c := buildTestCurve(t, DefaultConfig())
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	t.Run("存款报价精确重定价", func(t *testing.T) {
		for _, q := range testQuotes(now) {
			if q.Kind != marketquote.Deposit {
				continue
			}
			z, _ := c.ZeroRate(q.TenorYears)
			if math.Abs(z-q.Mid()) > 1e-11 {
				t.Fatalf("存款 %s 重定价失败: %.12f vs %.12f", q.ID, z, q.Mid())
			}
		}
		t.Logf("业务[曲线自洽] 用例[存款零息重定价] 结果=PASS")
	})

	t.Run("掉期报价平价重定价", func(t *testing.T) {
		for _, q := range testQuotes(now) {
			if q.Kind != marketquote.Swap {
				continue
			}
			par, err := ParSwapRate(c, q.TenorYears)
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(par-q.Mid()) > 1e-9 {
				t.Fatalf("掉期 %s 重定价失败: %.12f vs %.12f", q.ID, par, q.Mid())
			}
		}
		t.Logf("业务[曲线自洽] 用例[掉期平价重定价 残差<1e-9] 结果=PASS")
	})
}

func TestArbitrageFreeShape(t *testing.T) {
	c := buildTestCurve(t, DefaultConfig())
	_, last := c.TenorRange()
	prev := 1.0
	for i := 1; i <= 2000; i++ {
		tt := last * float64(i) / 2000
		df, err := c.DF(tt)
		if err != nil {
			t.Fatal(err)
		}
		if df > prev+1e-10 {
			t.Fatalf("区间内出现套利形态: DF(%.4f)=%.10f 上升", tt, df)
		}
		if df <= 0 || df > 1 {
			t.Fatalf("折现因子越界: %.10f", df)
		}
		prev = df
	}
	t.Logf("业务[无套利形态] 用例[报价区间内 DF 单调递减且在(0,1]] 结果=PASS")
}

func TestInterpolationAtArbitraryTenors(t *testing.T) {
	c := buildTestCurve(t, DefaultConfig())
	for _, tt := range []float64{0.33, 1.5, 2.7, 4.2, 7.8, 9.99} {
		z, err := c.ZeroRate(tt)
		if err != nil {
			t.Fatal(err)
		}
		if math.IsNaN(z) || math.IsInf(z, 0) {
			t.Fatalf("期限 %.2f 零息率非法: %v", tt, z)
		}
	}
	t.Logf("业务[任意期限查询] 用例[节点间线性零息插值] 结果=PASS")
}

func TestExtrapolationPolicies(t *testing.T) {
	t.Run("FLAT 外推稳定-边界零息持平", func(t *testing.T) {
		c := buildTestCurve(t, DefaultConfig())
		first, last := c.TenorRange()
		zShortEdge, _ := c.ZeroRate(first)
		zLongEdge, _ := c.ZeroRate(last)
		zFarShort, _ := c.ZeroRate(0.01)
		zFarLong, _ := c.ZeroRate(30)
		if zFarShort != zShortEdge || zFarLong != zLongEdge {
			t.Fatalf("FLAT 外推不稳定: %.12f/%.12f vs %.12f/%.12f",
				zFarShort, zFarLong, zShortEdge, zLongEdge)
		}
		t.Logf("业务[外推策略] 用例[FLAT 区间外零息持平] 结果=PASS")
	})

	t.Run("FAIL 外推显式报错", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Extrapolation = FailExtrap
		c := buildTestCurve(t, cfg)
		if _, err := c.DF(50); err == nil {
			t.Fatalf("超出报价区间且 FAIL 策略应报错")
		}
		if _, err := c.DF(0.01); err == nil {
			t.Fatalf("短端超出报价区间且 FAIL 策略应报错")
		}
		t.Logf("业务[外推策略] 用例[FAIL 区间外查询返回错误] 结果=PASS")
	})
}

func TestMissingAnchorFails(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ps := &marketquote.PreparedSet{AsOf: now, Swaps: testQuotes(now)[:1]}
	if _, err := Build(ps, DefaultConfig()); err == nil {
		t.Fatalf("没有存款锚点时必须构建失败")
	}
	t.Logf("业务[构建失败处理] 用例[缺少短端存款锚点] 结果=PASS")
}

func TestDeterministicRebuild(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ps, _, _ := marketquote.Validate(testQuotes(now), now, marketquote.DefaultPolicy())
	c1, err := Build(ps, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild from a shuffled copy of the same quotes.
	shuffled := append([]marketquote.Quote{}, ps.Deposits...)
	shuffled = append(shuffled, ps.Swaps...)
	shuffled[0], shuffled[len(shuffled)-1] = shuffled[len(shuffled)-1], shuffled[0]
	ps2, _, err := marketquote.Validate(shuffled, now, marketquote.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Build(ps2, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := range c1.nodes {
		if c1.nodes[i] != c2.nodes[i] {
			t.Fatalf("相同快照不同输入顺序导致曲线不一致，节点 %d: %+v vs %+v", i, c1.nodes[i], c2.nodes[i])
		}
	}
	t.Logf("业务[可复现] 用例[同快照乱序重建节点逐点一致] 结果=PASS")
}
