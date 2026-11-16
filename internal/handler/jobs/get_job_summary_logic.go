// Logic for specs/jobs/get-job-summary/spec.yaml — implemented by AI.

package jobs

import (
	"errors"

	"connectrpc.com/connect"

	"apifactory/gen/db"
	jobsv1 "apifactory/gen/factory/jobs/v1"
)

// getJobSummaryLogic 实现 GetJobSummary 的业务逻辑(式样书 behavior 节)。
// 纯函数: 不访问 DB/网络/文件,不用 context,不调用 time.Now,不用全局可变状态。
func getJobSummaryLogic(req *jobsv1.GetJobSummaryRequest, rows []db.GetJobSummaryRow) (*jobsv1.GetJobSummaryResponse, error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("getJobSummaryLogic: not implemented"))
}
