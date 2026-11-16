package jobs

import (
	"testing"
	"time"

	"apifactory/gen/db"
	jobsv1 "apifactory/gen/factory/jobs/v1"
)

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func row(t *testing.T, status int32, created string, finished string) db.GetJobSummaryRow {
	t.Helper()
	r := db.GetJobSummaryRow{Status: status, CreatedAt: ts(t, created)}
	if finished != "" {
		f := ts(t, finished)
		r.FinishedAt = &f
	}
	return r
}

func f64(v float64) *float64 { return &v }

type summaryWant struct {
	total, succeeded, failed, pending, sla int64
	rate, avg                              *float64
	health                                 string
}

func checkSummary(t *testing.T, got *jobsv1.GetJobSummaryResponse, w summaryWant) {
	t.Helper()
	if got.Total != w.total || got.Succeeded != w.succeeded || got.Failed != w.failed || got.Pending != w.pending {
		t.Errorf("counts = total %d succ %d fail %d pend %d, want %d %d %d %d",
			got.Total, got.Succeeded, got.Failed, got.Pending, w.total, w.succeeded, w.failed, w.pending)
	}
	if got.SlaBreaches != w.sla {
		t.Errorf("sla_breaches = %d, want %d", got.SlaBreaches, w.sla)
	}
	checkOpt(t, "success_rate", got.SuccessRate, w.rate)
	checkOpt(t, "avg_duration_seconds", got.AvgDurationSeconds, w.avg)
	if got.Health != w.health {
		t.Errorf("health = %q, want %q", got.Health, w.health)
	}
}

func checkOpt(t *testing.T, name string, got, want *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s = %v, want absent", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s absent, want %v", name, *want)
	case want != nil && *got != *want:
		t.Errorf("%s = %v, want %v", name, *got, *want)
	}
}

func TestGetJobSummaryLogic(t *testing.T) {
	alpha := func() []db.GetJobSummaryRow {
		var rs []db.GetJobSummaryRow
		base := ts(t, "2026-11-01T00:00:00Z")
		for i := 1; i <= 20; i++ {
			c := base.Add(time.Duration(i) * time.Minute)
			f := c.Add(time.Minute)
			st := int32(1)
			if i == 20 {
				st = 2
			}
			rs = append(rs, db.GetJobSummaryRow{ID: int64(i), Status: st, CreatedAt: c, FinishedAt: &f})
		}
		return rs
	}

	tests := []struct {
		name string
		rows []db.GetJobSummaryRow
		want summaryWant
	}{
		{"alpha", alpha(), summaryWant{20, 19, 1, 0, 0, f64(0.95), f64(60.0), "OK"}},
		{"beta", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-02T00:00:00Z", "2026-11-02T00:05:00Z"),
			row(t, 1, "2026-11-02T01:00:00Z", "2026-11-02T01:05:01Z"),
			row(t, 2, "2026-11-02T02:00:00Z", "2026-11-02T02:01:40Z"),
			row(t, 0, "2026-11-02T03:00:00Z", "2026-11-02T03:00:10Z"),
			row(t, 1, "2026-11-02T04:00:00Z", ""),
		}, summaryWant{5, 3, 1, 1, 1, f64(0.75), f64(233.7), "CRITICAL"}},
		{"gamma", []db.GetJobSummaryRow{
			row(t, 0, "2026-11-03T00:00:00Z", ""),
			row(t, 0, "2026-11-03T01:00:00Z", ""),
		}, summaryWant{2, 0, 0, 2, 0, nil, nil, "NO_DATA"}},
		{"delta", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-04T00:00:00Z", "2026-11-04T00:00:10Z"),
			row(t, 1, "2026-11-04T01:00:00Z", "2026-11-04T01:00:10Z"),
			row(t, 1, "2026-11-04T02:00:00Z", "2026-11-04T02:00:10Z"),
			row(t, 1, "2026-11-04T03:00:00Z", "2026-11-04T03:00:10Z"),
			row(t, 2, "2026-11-04T04:00:00Z", "2026-11-04T04:00:10Z"),
		}, summaryWant{5, 4, 1, 0, 0, f64(0.8), f64(10.0), "DEGRADED"}},
		{"epsilon", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-05T00:00:00Z", "2026-11-05T00:06:40Z"),
			row(t, 1, "2026-11-05T01:00:00Z", "2026-11-05T01:00:10Z"),
		}, summaryWant{2, 2, 0, 0, 1, f64(1.0), f64(205.0), "DEGRADED"}},
		{"zeta", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-06T00:00:00Z", "2026-11-06T00:00:01Z"),
			row(t, 1, "2026-11-06T01:00:00Z", "2026-11-06T01:00:02Z"),
			row(t, 2, "2026-11-06T02:00:00Z", "2026-11-06T02:00:02Z"),
		}, summaryWant{3, 2, 1, 0, 0, f64(0.6667), f64(1.7), "CRITICAL"}},
		{"eta", []db.GetJobSummaryRow{
			row(t, 3, "2026-11-07T00:00:00Z", "2026-11-07T00:00:10Z"),
			row(t, 1, "2026-11-07T01:00:00Z", "2026-11-07T00:59:00Z"),
			row(t, 2, "2026-11-07T02:00:00Z", "2026-11-07T02:10:00Z"),
			row(t, 1, "2026-11-07T03:00:00Z", "2026-11-07T03:00:20Z"),
		}, summaryWant{3, 2, 1, 0, 1, f64(0.6667), f64(310.0), "CRITICAL"}},
		{"theta", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-07T04:00:00Z", ""),
			row(t, 1, "2026-11-07T05:00:00Z", ""),
		}, summaryWant{2, 2, 0, 0, 0, f64(1.0), nil, "OK"}},
		{"iota", []db.GetJobSummaryRow{
			row(t, 5, "2026-11-07T06:00:00Z", "2026-11-07T06:00:01Z"),
		}, summaryWant{0, 0, 0, 0, 0, nil, nil, "NO_DATA"}},
		{"kappa", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-07T07:00:00Z", "2026-11-07T07:00:00.5Z"),
			row(t, 2, "2026-11-07T07:10:00Z", "2026-11-07T07:10:00Z"),
		}, summaryWant{2, 1, 1, 0, 0, f64(0.5), f64(0.3), "CRITICAL"}},
		{"lambda", []db.GetJobSummaryRow{
			row(t, 2, "2026-11-07T08:00:00Z", "2026-11-07T08:00:01Z"),
		}, summaryWant{1, 0, 1, 0, 0, f64(0.0), f64(1.0), "CRITICAL"}},
		{"mu", []db.GetJobSummaryRow{
			row(t, 1, "2026-11-07T09:00:00Z", "2026-11-07T09:05:00.5Z"),
		}, summaryWant{1, 1, 0, 0, 1, f64(1.0), f64(300.5), "DEGRADED"}},
		{"empty", nil, summaryWant{0, 0, 0, 0, 0, nil, nil, "NO_DATA"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getJobSummaryLogic(&jobsv1.GetJobSummaryRequest{}, tt.rows)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			checkSummary(t, got, tt.want)
		})
	}
}

// health 用四舍五入前的 success_rate 判定(review_decisions: 由单元测试保证)。
func TestGetJobSummaryLogic_HealthUsesUnroundedRate(t *testing.T) {
	gen := func(succeeded, failed int) []db.GetJobSummaryRow {
		c := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
		f := c.Add(10 * time.Second)
		rs := make([]db.GetJobSummaryRow, 0, succeeded+failed)
		for i := 0; i < succeeded; i++ {
			rs = append(rs, db.GetJobSummaryRow{Status: 1, CreatedAt: c, FinishedAt: &f})
		}
		for i := 0; i < failed; i++ {
			rs = append(rs, db.GetJobSummaryRow{Status: 2, CreatedAt: c, FinishedAt: &f})
		}
		return rs
	}
	tests := []struct {
		name              string
		succeeded, failed int
		rate              float64
		health            string
	}{
		// 37999/40000 = 0.949975 → 0.95 显示,但 < 0.95 → DEGRADED
		{"just below 0.95", 37999, 2001, 0.95, "DEGRADED"},
		// 19999/25000 = 0.79996 → 0.8 显示,但 < 0.80 → CRITICAL
		{"just below 0.80", 19999, 5001, 0.8, "CRITICAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getJobSummaryLogic(&jobsv1.GetJobSummaryRequest{}, gen(tt.succeeded, tt.failed))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got.SuccessRate == nil || *got.SuccessRate != tt.rate {
				t.Errorf("success_rate = %v, want %v", got.SuccessRate, tt.rate)
			}
			if got.Health != tt.health {
				t.Errorf("health = %q, want %q", got.Health, tt.health)
			}
		})
	}
}
