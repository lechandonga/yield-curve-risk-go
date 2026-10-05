package marketquote

import (
	"errors"
	"testing"
	"time"
)

func mkQuote(id string, kind InstrumentKind, tenor, bid, ask float64, at time.Time) Quote {
	return Quote{ID: id, Kind: kind, TenorYears: tenor, BidRate: bid, AskRate: ask, QuoteTime: at}
}

func validBatch(now time.Time) []Quote {
	return []Quote{
		mkQuote("d3m", Deposit, 0.25, 0.028, 0.0282, now.Add(-time.Minute)),
		mkQuote("d6m", Deposit, 0.5, 0.030, 0.0302, now.Add(-time.Minute)),
		mkQuote("d1y", Deposit, 1, 0.032, 0.0322, now.Add(-time.Minute)),
		mkQuote("s2y", Swap, 2, 0.035, 0.0352, now.Add(-2*time.Minute)),
		mkQuote("s5y", Swap, 5, 0.04, 0.0402, now.Add(-2*time.Minute)),
	}
}

func TestQuoteValidationCases(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	policy := DefaultPolicy()

	tests := []struct {
		name       string
		mutate     func(qs []Quote) []Quote
		wantReason RejectReason
		fatal      bool
	}{
		{
			name: "字段缺失-空ID",
			mutate: func(qs []Quote) []Quote {
				qs[0].ID = "  "
				return qs
			},
			wantReason: MissingID, fatal: true,
		},
		{
			name: "未知报价类型",
			mutate: func(qs []Quote) []Quote {
				qs[0].Kind = "FRA"
				return qs
			},
			wantReason: UnknownKind, fatal: true,
		},
		{
			name: "期限非法-零期限",
			mutate: func(qs []Quote) []Quote {
				qs[0].TenorYears = 0
				return qs
			},
			wantReason: InvalidTenor, fatal: true,
		},
		{
			name: "期限非法-NaN",
			mutate: func(qs []Quote) []Quote {
				qs[0].TenorYears = nanf()
				return qs
			},
			wantReason: InvalidTenor, fatal: true,
		},
		{
			name: "买卖价倒挂",
			mutate: func(qs []Quote) []Quote {
				qs[0].BidRate, qs[0].AskRate = 0.031, 0.030
				return qs
			},
			wantReason: InvertedQuote, fatal: true,
		},
		{
			name: "非有限利率",
			mutate: func(qs []Quote) []Quote {
				qs[0].AskRate = inff()
				return qs
			},
			wantReason: NonFiniteRate, fatal: true,
		},
		{
			name: "负利率点差(负报价)",
			mutate: func(qs []Quote) []Quote {
				qs[0].BidRate = -0.001
				return qs
			},
			wantReason: NegativeSpread, fatal: true,
		},
		{
			name: "行情过期",
			mutate: func(qs []Quote) []Quote {
				qs[0].QuoteTime = now.Add(-time.Hour)
				return qs
			},
			wantReason: StaleQuote, fatal: true,
		},
		{
			name: "行情时间戳在未来",
			mutate: func(qs []Quote) []Quote {
				qs[0].QuoteTime = now.Add(time.Hour)
				return qs
			},
			wantReason: FutureQuote, fatal: true,
		},
		{
			name: "重复ID",
			mutate: func(qs []Quote) []Quote {
				qs = append(qs, mkQuote("d3m", Deposit, 0.75, 0.031, 0.0312, now.Add(-time.Minute)))
				return qs
			},
			wantReason: DuplicateID, fatal: true,
		},
		{
			name: "同期限冲突报价",
			mutate: func(qs []Quote) []Quote {
				qs = append(qs, mkQuote("d3m_bad", Deposit, 0.25, 0.039, 0.0392, now.Add(-time.Minute)))
				return qs
			},
			wantReason: ConflictQuote, fatal: true,
		},
		{
			name: "完全重复报价-非致命-首条保留",
			mutate: func(qs []Quote) []Quote {
				dup := qs[0]
				qs = append(qs, dup)
				return qs
			},
			wantReason: DuplicateQuote, fatal: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			qs := tc.mutate(validBatch(now))
			prepared, rejected, err := Validate(qs, now, policy)
			found := findReason(rejected, tc.wantReason)
			if !found {
				t.Fatalf("期望拒绝原因 %s，实际拒绝记录: %v", tc.wantReason, rejected)
			}
			if tc.fatal {
				var batchErr *BatchError
				if !errors.As(err, &batchErr) {
					t.Fatalf("期望批次失败 *BatchError，实际 err=%v", err)
				}
				if prepared == nil {
					t.Fatalf("即使失败也应返回 prepared 用于解释")
				}
			} else {
				if err != nil {
					t.Fatalf("非致命重复不应使批次失败，实际 err=%v", err)
				}
			}
			t.Logf("业务[脏报价处理] 用例[%s] 结果=PASS 原因=%s 致命=%v", tc.name, tc.wantReason, tc.fatal)
		})
	}
}

func TestDuplicateResolutionIsDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	base := validBatch(now)

	run := func(qs []Quote) *PreparedSet {
		p, _, err := Validate(qs, now, DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	dup := base[0]
	withTail := append(append([]Quote{}, base...), dup)
	withHead := append(append([]Quote{}, dup), base...)
	p1, p2 := run(withTail), run(withHead)
	if len(p1.Deposits) != len(p2.Deposits) {
		t.Fatalf("不同输入顺序下解析结果不一致: %d vs %d", len(p1.Deposits), len(p2.Deposits))
	}
	t.Logf("业务[确定性规则] 用例[重复报价与输入顺序无关] 结果=PASS 存款报价数=%d", len(p1.Deposits))
}

func TestCleanBatchAccepted(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ps, rejected, err := Validate(validBatch(now), now, DefaultPolicy())
	if err != nil {
		t.Fatalf("干净批次不应被拒绝: %v", err)
	}
	if len(rejected) != 0 || len(ps.Deposits) != 3 || len(ps.Swaps) != 2 {
		t.Fatalf("干净批次结果异常: rejected=%v deposits=%d swaps=%d", rejected, len(ps.Deposits), len(ps.Swaps))
	}
	t.Logf("业务[干净批次] 用例[正常报价全部接受] 结果=PASS")
}

func findReason(rs []RejectRecord, want RejectReason) bool {
	for _, r := range rs {
		if r.Reason == want {
			return true
		}
	}
	return false
}

func nanf() float64 {
	var z float64
	return z / z
}

func inff() float64 {
	z := 1.0
	return z / 0
}
