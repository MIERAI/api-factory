package jobs

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"apifactory/gen/db"
	jobsv1 "apifactory/gen/factory/jobs/v1"
	"apifactory/platform/apierr"
)

const defaultListJobsPageSize = 20

// ListJobs 按条件列出任务(新的在前,keyset 游标分页)。
// 式样书: specs/jobs/list-jobs/spec.yaml
//
// page_size / status / job_type 的范围和 created_after < created_before
// 由 protovalidate 自动处理;DB 错误原样返回,由平台拦截器转换。
func (h *Handler) ListJobs(
	ctx context.Context,
	req *connect.Request[jobsv1.ListJobsRequest],
) (*connect.Response[jobsv1.ListJobsResponse], error) {
	msg := req.Msg

	pageSize := msg.GetPageSize()
	if pageSize == 0 {
		pageSize = defaultListJobsPageSize
	}

	params := db.ListJobsParams{
		Status:   msg.Status,
		JobType:  msg.JobType,
		RowLimit: pageSize + 1, // 多取 1 行,只用于判断是否有下一页
	}
	if msg.CreatedAfter != nil {
		t := msg.CreatedAfter.AsTime()
		params.CreatedAfter = &t
	}
	if msg.CreatedBefore != nil {
		t := msg.CreatedBefore.AsTime()
		params.CreatedBefore = &t
	}
	if token := msg.GetPageToken(); token != "" {
		createdAt, id, err := decodeListJobsPageToken(token)
		if err != nil {
			return nil, apierr.InvalidArgument("invalid page_token")
		}
		params.CursorCreatedAt = &createdAt
		params.CursorID = &id
	}

	rows, err := h.q.ListJobs(ctx, params)
	if err != nil {
		return nil, err
	}

	hasNext := len(rows) > int(pageSize)
	if hasNext {
		rows = rows[:pageSize]
	}

	res := &jobsv1.ListJobsResponse{Jobs: make([]*jobsv1.Job, 0, len(rows))}
	for _, r := range rows {
		res.Jobs = append(res.Jobs, &jobsv1.Job{
			Id:        r.ID,
			JobType:   r.JobType,
			Status:    r.Status,
			CreatedAt: timestamppb.New(r.CreatedAt),
		})
	}
	if hasNext {
		last := rows[len(rows)-1]
		res.NextPageToken = encodeListJobsPageToken(last.CreatedAt, last.ID)
	}

	return connect.NewResponse(res), nil
}

// page_token 格式: base64url( "<created_at RFC3339Nano UTC>|<id>" )。
// 调用方视为不透明字符串。

func encodeListJobsPageToken(createdAt time.Time, id int64) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

var errInvalidPageToken = errors.New("invalid page_token")

func decodeListJobsPageToken(token string) (time.Time, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, 0, errInvalidPageToken
	}
	tsPart, idPart, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, 0, errInvalidPageToken
	}
	createdAt, err := time.Parse(time.RFC3339Nano, tsPart)
	if err != nil {
		return time.Time{}, 0, errInvalidPageToken
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return time.Time{}, 0, errInvalidPageToken
	}
	return createdAt.UTC(), id, nil
}
