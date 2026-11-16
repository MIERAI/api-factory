// Logic for specs/jobs/get-job-summary/spec.yaml — implemented by AI.

package jobs

import (
	"math"

	"apifactory/gen/db"
	jobsv1 "apifactory/gen/factory/jobs/v1"
)

const (
	jobStatusPending   int32 = 0
	jobStatusSucceeded int32 = 1
	jobStatusFailed    int32 = 2

	slaThresholdSeconds = 300.0
)

// getJobSummaryLogic 实现 GetJobSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getJobSummaryLogic(req *jobsv1.GetJobSummaryRequest, rows []db.GetJobSummaryRow) (*jobsv1.GetJobSummaryResponse, error) {
	resp := &jobsv1.GetJobSummaryResponse{}

	var durationSum float64
	var durationCount int64

	for _, r := range rows {
		switch r.Status {
		case jobStatusPending:
			// finished_at 非 NULL 也按 pending 计,忽略 finished_at
			resp.Pending++
			resp.Total++
			continue
		case jobStatusSucceeded:
			resp.Succeeded++
		case jobStatusFailed:
			resp.Failed++
		default:
			// 未知 status: 从所有统计中排除(total 也不计)
			continue
		}
		resp.Total++

		// 已结束: finished_at 为 NULL 或处理时间为负的任务不参与平均和 SLA
		if r.FinishedAt == nil {
			continue
		}
		d := r.FinishedAt.Sub(r.CreatedAt).Seconds()
		if d < 0 {
			continue
		}
		durationSum += d
		durationCount++
		if d > slaThresholdSeconds {
			resp.SlaBreaches++
		}
	}

	finished := resp.Succeeded + resp.Failed

	var rawRate float64
	if finished > 0 {
		rawRate = float64(resp.Succeeded) / float64(finished)
		rate := roundTo(rawRate, 4)
		resp.SuccessRate = &rate
	}

	if durationCount > 0 {
		avg := roundTo(durationSum/float64(durationCount), 1)
		resp.AvgDurationSeconds = &avg
	}

	// health: 使用四舍五入前的 success_rate 判定
	switch {
	case finished == 0:
		resp.Health = "NO_DATA"
	case rawRate >= 0.95 && resp.SlaBreaches == 0:
		resp.Health = "OK"
	case rawRate >= 0.80:
		resp.Health = "DEGRADED"
	default:
		resp.Health = "CRITICAL"
	}

	return resp, nil
}

// roundTo 按式样书: math.Round(x * 10^n) / 10^n(0.5 远离零)。
func roundTo(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
