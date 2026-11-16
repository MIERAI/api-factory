// Logic for specs/jobs/get-job-summary/spec.yaml — implemented by AI.

package jobs

import (
	"math"

	jobsv1 "apifactory/gen/factory/jobs/v1"
)

const (
	jobSummarySLASeconds = 300.0

	jobHealthOK       = "OK"
	jobHealthDegraded = "DEGRADED"
	jobHealthCritical = "CRITICAL"
	jobHealthNoData   = "NO_DATA"
)

// getJobSummaryLogic 实现 GetJobSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getJobSummaryLogic(req *jobsv1.GetJobSummaryRequest, rows []getJobSummaryRow) (*jobsv1.GetJobSummaryResponse, error) {
	_ = req // 筛选已由查询完成,逻辑不依赖请求参数

	res := &jobsv1.GetJobSummaryResponse{}

	var durationSum float64
	var durationCount int64

	for _, r := range rows {
		switch r.Status {
		case jobsv1.JobStatus_JOB_STATUS_PENDING:
			// PENDING 但 finished_at 非 NULL: 按 pending 计,忽略 finished_at
			res.Pending++
			res.Total++
			continue
		case jobsv1.JobStatus_JOB_STATUS_SUCCEEDED:
			res.Succeeded++
			res.Total++
		case jobsv1.JobStatus_JOB_STATUS_FAILED:
			res.Failed++
			res.Total++
		default:
			// UNSPECIFIED(平台读到不认识的 DB 值): 从所有统计中排除
			continue
		}

		// 已结束(SUCCEEDED / FAILED)
		if r.FinishedAt == nil {
			// 数据不一致: 不参与平均和 SLA
			continue
		}
		d := r.FinishedAt.Sub(r.CreatedAt).Seconds()
		if d < 0 {
			// 处理时间为负(时钟偏差): 不参与平均和 SLA
			continue
		}
		durationSum += d
		durationCount++
		if d > jobSummarySLASeconds {
			res.SlaBreaches++
		}
	}

	finished := res.Succeeded + res.Failed
	var rate float64
	if finished > 0 {
		rate = float64(res.Succeeded) / float64(finished)
		rounded := jobSummaryRound(rate, 4)
		res.SuccessRate = &rounded
	}
	if durationCount > 0 {
		avg := jobSummaryRound(durationSum/float64(durationCount), 1)
		res.AvgDurationSeconds = &avg
	}

	// health: 使用四舍五入前的 success_rate
	switch {
	case finished == 0:
		res.Health = jobHealthNoData
	case rate >= 0.95 && res.SlaBreaches == 0:
		res.Health = jobHealthOK
	case rate >= 0.80:
		res.Health = jobHealthDegraded
	default:
		res.Health = jobHealthCritical
	}

	return res, nil
}

// jobSummaryRound: math.Round(x * 10^n) / 10^n(0.5 远离零)
func jobSummaryRound(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}
