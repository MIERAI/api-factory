package jobs

import (
	"context"

	"connectrpc.com/connect"

	jobsv1 "apifactory/gen/factory/jobs/v1"
	"apifactory/platform/apierr"
)

// GetJobStatus 按 id 返回一个异步任务的处理状态。
// 式样书: specs/jobs/get-job-status/spec.yaml
//
// job_id 的校验(> 0)由 protovalidate 自动处理;
// DB 错误原样返回,由平台拦截器转换成 UNAVAILABLE / INTERNAL。
func (h *Handler) GetJobStatus(
	ctx context.Context,
	req *connect.Request[jobsv1.GetJobStatusRequest],
) (*connect.Response[jobsv1.GetJobStatusResponse], error) {
	jobID := req.Msg.GetJobId()

	status, err := h.q.GetJobStatus(ctx, jobID)
	if err != nil {
		if apierr.IsNoRows(err) {
			return nil, apierr.NotFound("job not found: %d", jobID)
		}
		return nil, err
	}

	return connect.NewResponse(&jobsv1.GetJobStatusResponse{Status: status}), nil
}
