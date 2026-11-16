// Package conv 是 proto 类型和查询类型之间的转换。生成的映射代码使用,AI 不需要直接调用。
package conv

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// TimePtr: 未指定(nil)→ nil(查询里 NULL = 不筛选)
func TimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

// TS: time.Time → Timestamp
func TS(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}

// TSPtr: 可空列 → Timestamp(NULL → 未设置)
func TSPtr(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
