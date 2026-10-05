package registry

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lechandonga/yield-curve-risk-go/internal/curve"
	"github.com/lechandonga/yield-curve-risk-go/internal/instrument"
	"github.com/lechandonga/yield-curve-risk-go/internal/marketquote"
	"github.com/lechandonga/yield-curve-risk-go/internal/risk"
)

func baseQuotes(level float64, now time.Time) []marketquote.Quote {
	return []marketquote.Quote{
		{ID: fmt.Sprintf("dep3m-%.0f", level*1e6), Kind: marketquote.Deposit, TenorYears: 0.25,
			BidRate: 0.028 + level, AskRate: 0.0282 + level, QuoteTime: now.Add(-time.Minute)},
		{ID: fmt.Sprintf("dep1y-%.0f", level*1e6), Kind: marketquote.Deposit, TenorYears: 1,
			BidRate: 0.032 + level, AskRate: 0.0322 + level, QuoteTime: now.Add(-time.Minute)},
		{ID: fmt.Sprintf("swp5y-%.0f", level*1e6), Kind: marketquote.Swap, TenorYears: 5,
			BidRate: 0.040 + level, AskRate: 0.0402 + level, QuoteTime: now.Add(-2 * time.Minute)},
		{ID: fmt.Sprintf("swp10y-%.0f", level*1e6), Kind: marketquote.Swap, TenorYears: 10,
			BidRate: 0.045 + level, AskRate: 0.0452 + level, QuoteTime: now.Add(-2 * time.Minute)},
	}
}

func mustPublish(t *testing.T, r *Registry, level float64, now time.Time) int {
	t.Helper()
	v, err := r.Update(baseQuotes(level, now), now)
	if err != nil {
		t.Fatalf("合格批次发布失败: %v", err)
	}
	return v.Number
}

func TestPublishAndHistory(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := New(DefaultConfig(), nil)

	n1 := mustPublish(t, r, 0, now)
	n2 := mustPublish(t, r, 0.002, now.Add(time.Minute))
	if n1 != 1 || n2 != 2 || r.Current().Number != 2 {
		t.Fatalf("版本号管理异常: %d %d current=%d", n1, n2, r.Current().Number)
	}
	v1, err := r.Get(1)
	if err != nil || v1.Number != 1 {
		t.Fatalf("历史版本查询失败: %v", err)
	}
	if _, err := r.Get(99); err == nil {
		t.Fatalf("不存在的版本应报错")
	}

	// An old version must stay immutable and re-price identically.
	inst := instrument.Bond("b", 0.04, 1e6, 5)
	pv1a, _ := inst.PV(v1.Curve)
	pv1b, _ := inst.PV(v1.Curve)
	if pv1a != pv1b {
		t.Fatalf("历史版本不可变性被破坏")
	}
	if pv1a == mustPV(t, r.Current().Curve, inst) {
		t.Fatalf("不同版本 PV 应不同")
	}
	t.Logf("业务[版本管理] 用例[发布/当前/历史/不可变] 结果=PASS 版本1 PV=%.4f 版本2 PV=%.4f",
		pv1a, mustPV(t, r.Current().Curve, inst))
}

func mustPV(t *testing.T, c *curve.Curve, inst instrument.FixedCashFlows) float64 {
	t.Helper()
	pv, err := inst.PV(c)
	if err != nil {
		t.Fatal(err)
	}
	return pv
}

func TestFailedBatchKeepsPreviousVersion(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := New(DefaultConfig(), nil)
	mustPublish(t, r, 0, now)

	t.Run("脏数据批次被整体拒绝", func(t *testing.T) {
		bad := baseQuotes(0.001, now)
		bad[0].BidRate = 0.99 // inverted
		_, err := r.Update(bad, now)
		var ue *UpdateError
		if !errors.As(err, &ue) {
			t.Fatalf("应返回 *UpdateError，实际 %v", err)
		}
		if r.Current().Number != 1 {
			t.Fatalf("失败批次不得改变当前版本，实际版本 %d", r.Current().Number)
		}
		if len(ue.Rejects) == 0 {
			t.Fatalf("失败原因必须暴露")
		}
		t.Logf("业务[失败回退] 用例[倒挂报价-保留v1] 结果=PASS 原因=%s", ue.Rejects[0].Reason)
	})

	t.Run("过期批次被拒绝", func(t *testing.T) {
		old := baseQuotes(0.003, now.Add(-time.Hour))
		_, err := r.Update(old, now)
		if err == nil || r.Current().Number != 1 {
			t.Fatalf("过期批次必须被拒绝且保留 v1, err=%v", err)
		}
		t.Logf("业务[失败回退] 用例[行情过期-保留v1] 结果=PASS")
	})

	t.Run("冲突报价被拒绝", func(t *testing.T) {
		conflict := baseQuotes(0.005, now)
		dup := conflict[0]
		dup.ID = "another-id"
		dup.BidRate += 0.01
		dup.AskRate += 0.01
		conflict = append(conflict, dup)
		_, err := r.Update(conflict, now)
		if err == nil || r.Current().Number != 1 {
			t.Fatalf("冲突批次必须被拒绝且保留 v1, err=%v", err)
		}
		t.Logf("业务[失败回退] 用例[同期限冲突-保留v1] 结果=PASS")
	})

	// After all failures, a good batch publishes as v2 (not v5): failed
	// batches must not consume version numbers or partially apply.
	n := mustPublish(t, r, 0.002, now)
	if n != 2 {
		t.Fatalf("失败批次消耗了版本号或部分应用，下一版本=%d", n)
	}
	t.Logf("业务[失败回退] 用例[失败后成功批次为v2-无部分应用] 结果=PASS")
}

func TestConcurrentUpdatesReadsAndHistory(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := New(DefaultConfig(), nil)
	mustPublish(t, r, 0, now)

	inst := instrument.Bond("b10", 0.04, 1e6, 10)
	keys := []float64{1, 5, 10}

	const updaters, readers = 8, 16
	var wg sync.WaitGroup
	var readerFailures int64

	// Updaters: alternating good and bad batches.
	for u := 0; u < updaters; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			for k := 0; k < 25; k++ {
				batchNow := now.Add(time.Duration(u*100+k) * time.Second)
				if k%2 == 0 {
					bad := baseQuotes(float64(u)*0.0003+0.004, batchNow)
					bad[1].AskRate = bad[1].BidRate - 0.01 // inverted
					_, _ = r.Update(bad, batchNow)
				} else {
					_, _ = r.Update(baseQuotes(float64(u)*0.0003+float64(k)*0.0001, batchNow), batchNow)
				}
			}
		}(u)
	}

	// Readers must always see a complete, internally consistent version.
	for rd := 0; rd < readers; rd++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 300; k++ {
				v := r.Current()
				if v == nil || v.Curve == nil {
					atomic.AddInt64(&readerFailures, 1)
					return
				}
				basePV, err := inst.PV(v.Curve)
				if err != nil {
					t.Errorf("reader PV error on v%d: %v", v.Number, err)
					atomic.AddInt64(&readerFailures, 1)
					return
				}
				// Risk reconciliation on the published snapshot must hold,
				// proving readers never observe a half-built curve.
				res, err := risk.Analyze(inst, v.Curve, keys, risk.DefaultConfig())
				if err != nil {
					t.Errorf("reader Analyze error on v%d: %v", v.Number, err)
					atomic.AddInt64(&readerFailures, 1)
					return
				}
				if d := mathAbs(res.SumKeyDelta - res.ParallelDelta); d > 1e-8+1e-9*mathAbs(basePV) {
					t.Errorf("reader reconcile gap on v%d: %.3e", v.Number, d)
					atomic.AddInt64(&readerFailures, 1)
					return
				}
				// Historical lookup for the current number must agree.
				hv, err := r.Get(v.Number)
				if err != nil || hv != v {
					t.Errorf("reader history mismatch v%d err=%v", v.Number, err)
					atomic.AddInt64(&readerFailures, 1)
					return
				}
			}
		}()
	}

	wg.Wait()
	if atomic.LoadInt64(&readerFailures) != 0 {
		t.Fatalf("并发读取发现 %d 次不一致/半更新状态", readerFailures)
	}

	versions := r.Versions()
	for i, meta := range versions {
		if meta.Number != i+1 {
			t.Fatalf("历史版本号不连续: 位置%d 编号%d", i, meta.Number)
		}
	}
	if len(versions) < 2 {
		t.Fatalf("应当至少有若干成功版本，实际 %d", len(versions))
	}
	t.Logf("业务[并发一致性] 用例[%d更新者×25批(半数坏批)+%d读者×300次对账] 结果=PASS 成功版本数=%d 零半更新",
		updaters, readers, len(versions))
}

func TestDuplicateQuoteBatchStillPublishesWithWarning(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := New(DefaultConfig(), nil)
	qs := baseQuotes(0, now)
	qs = append(qs, qs[0]) // exact duplicate
	v, err := r.Update(qs, now)
	if err != nil {
		t.Fatalf("完全重复不应阻止发布: %v", err)
	}
	if len(v.Warnings) != 1 || v.Warnings[0].Reason != marketquote.DuplicateQuote {
		t.Fatalf("重复报价应作为 warning 暴露，实际 %+v", v.Warnings)
	}
	t.Logf("业务[确定性去重] 用例[完全重复发布但带warning] 结果=PASS warning=%s", v.Warnings[0].Reason)
}

func mathAbs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
