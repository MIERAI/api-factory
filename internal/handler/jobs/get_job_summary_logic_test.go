package jobs

import (
	"testing"

	jobsv1 "apifactory/gen/factory/jobs/v1"
)

const (
	tSucc = jobsv1.JobStatus_JOB_STATUS_SUCCEEDED
	tFail = jobsv1.JobStatus_JOB_STATUS_FAILED
	tPend = jobsv1.JobStatus_JOB_STATUS_PENDING
)

func jsRow(st jobsv1.JobStatus, created, finished string) getJobSummaryRow {
	r := getJobSummaryRow{Status: st, CreatedAt: mustTime(created)}
	if finished != "" {
		r.FinishedAt = ptrOf(mustTime(finished))
	}
	return r
}

type jsWant struct {
	total, succ, fail, pend, sla int64
	rate, avg                    *float64
	health                       string
}

func jsF(v float64) *float64 { return &v }

func jsCheckFloatPtr(t *testing.T, name string, want, got *float64) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("%s: want unset, got %v", name, *got)
	case want != nil && got == nil:
		t.Errorf("%s: want %v, got unset", name, *want)
	case want != nil && *want != *got:
		t.Errorf("%s: want %v, got %v", name, *want, *got)
	}
}

func Test_getJobSummaryLogic_Acceptance(t *testing.T) {
	cases := []struct {
		name string
		rows []getJobSummaryRow
		want jsWant
	}{
		{
			name: "beta: 300s 不违约 / 301s 违约 / pending 有 finished_at / 成功无 finished_at",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-02T00:00:00Z", "2026-11-02T00:05:00Z"),
				jsRow(tSucc, "2026-11-02T01:00:00Z", "2026-11-02T01:05:01Z"),
				jsRow(tFail, "2026-11-02T02:00:00Z", "2026-11-02T02:01:40Z"),
				jsRow(tPend, "2026-11-02T03:00:00Z", "2026-11-02T03:00:10Z"),
				jsRow(tSucc, "2026-11-02T04:00:00Z", ""),
			},
			want: jsWant{total: 5, succ: 3, fail: 1, pend: 1, sla: 1, rate: jsF(0.75), avg: jsF(233.7), health: "CRITICAL"},
		},
		{
			name: "gamma: 全部 pending → NO_DATA",
			rows: []getJobSummaryRow{
				jsRow(tPend, "2026-11-03T00:00:00Z", ""),
				jsRow(tPend, "2026-11-03T01:00:00Z", ""),
			},
			want: jsWant{total: 2, pend: 2, health: "NO_DATA"},
		},
		{
			name: "delta: 0.80 刚好 DEGRADED",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-04T00:00:00Z", "2026-11-04T00:00:10Z"),
				jsRow(tSucc, "2026-11-04T01:00:00Z", "2026-11-04T01:00:10Z"),
				jsRow(tSucc, "2026-11-04T02:00:00Z", "2026-11-04T02:00:10Z"),
				jsRow(tSucc, "2026-11-04T03:00:00Z", "2026-11-04T03:00:10Z"),
				jsRow(tFail, "2026-11-04T04:00:00Z", "2026-11-04T04:00:10Z"),
			},
			want: jsWant{total: 5, succ: 4, fail: 1, rate: jsF(0.8), avg: jsF(10), health: "DEGRADED"},
		},
		{
			name: "epsilon: 成功率 1.0 但有违约 → DEGRADED",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-05T00:00:00Z", "2026-11-05T00:06:40Z"),
				jsRow(tSucc, "2026-11-05T01:00:00Z", "2026-11-05T01:00:10Z"),
			},
			want: jsWant{total: 2, succ: 2, sla: 1, rate: jsF(1), avg: jsF(205), health: "DEGRADED"},
		},
		{
			name: "zeta: 2/3 → 0.6667, 1.666… → 1.7",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-06T00:00:00Z", "2026-11-06T00:00:01Z"),
				jsRow(tSucc, "2026-11-06T01:00:00Z", "2026-11-06T01:00:02Z"),
				jsRow(tFail, "2026-11-06T02:00:00Z", "2026-11-06T02:00:02Z"),
			},
			want: jsWant{total: 3, succ: 2, fail: 1, rate: jsF(0.6667), avg: jsF(1.7), health: "CRITICAL"},
		},
		{
			name: "eta: 负处理时间排除 / 失败也算违约",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-07T01:00:00Z", "2026-11-07T00:59:00Z"),
				jsRow(tFail, "2026-11-07T02:00:00Z", "2026-11-07T02:10:00Z"),
				jsRow(tSucc, "2026-11-07T03:00:00Z", "2026-11-07T03:00:20Z"),
			},
			want: jsWant{total: 3, succ: 2, fail: 1, sla: 1, rate: jsF(0.6667), avg: jsF(310), health: "CRITICAL"},
		},
		{
			name: "theta: finished_at 全部 NULL → avg 不设置, health OK",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-07T04:00:00Z", ""),
				jsRow(tSucc, "2026-11-07T05:00:00Z", ""),
			},
			want: jsWant{total: 2, succ: 2, rate: jsF(1), health: "OK"},
		},
		{
			name: "kappa: 小数秒 / 0 秒有效 / 0.25 → 0.3",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-07T07:00:00Z", "2026-11-07T07:00:00.5Z"),
				jsRow(tFail, "2026-11-07T07:10:00Z", "2026-11-07T07:10:00Z"),
			},
			want: jsWant{total: 2, succ: 1, fail: 1, rate: jsF(0.5), avg: jsF(0.3), health: "CRITICAL"},
		},
		{
			name: "lambda: 全部失败 → success_rate = 0.0",
			rows: []getJobSummaryRow{
				jsRow(tFail, "2026-11-07T08:00:00Z", "2026-11-07T08:00:01Z"),
			},
			want: jsWant{total: 1, fail: 1, rate: jsF(0), avg: jsF(1), health: "CRITICAL"},
		},
		{
			name: "mu: 300.5 秒算违约",
			rows: []getJobSummaryRow{
				jsRow(tSucc, "2026-11-07T09:00:00Z", "2026-11-07T09:05:00.5Z"),
			},
			want: jsWant{total: 1, succ: 1, sla: 1, rate: jsF(1), avg: jsF(300.5), health: "DEGRADED"},
		},
		{
			name: "空输入 → NO_DATA",
			rows: nil,
			want: jsWant{health: "NO_DATA"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := getJobSummaryLogic(&jobsv1.GetJobSummaryRequest{}, tc.rows)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			w := tc.want
			if res.GetTotal() != w.total || res.GetSucceeded() != w.succ || res.GetFailed() != w.fail ||
				res.GetPending() != w.pend || res.GetSlaBreaches() != w.sla {
				t.Errorf("counts: want total=%d succ=%d fail=%d pend=%d sla=%d, got total=%d succ=%d fail=%d pend=%d sla=%d",
					w.total, w.succ, w.fail, w.pend, w.sla,
					res.GetTotal(), res.GetSucceeded(), res.GetFailed(), res.GetPending(), res.GetSlaBreaches())
			}
			jsCheckFloatPtr(t, "success_rate", w.rate, res.SuccessRate)
			jsCheckFloatPtr(t, "avg_duration_seconds", w.avg, res.AvgDurationSeconds)
			if res.GetHealth() != w.health {
				t.Errorf("health: want %s, got %s", w.health, res.GetHealth())
			}
		})
	}
}
