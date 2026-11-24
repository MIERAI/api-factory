package tracing

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// NewInterceptor: Connect 的每个请求一个 server span(名 = 过程名去掉开头的 "/",例 order.order.v1.OrderService/PlaceOrder),
// 父 = 请求头 traceparent。server 只在追踪打开时装它(最外层: 日志、超时、校验都在 span 里)。
//
// 状态: 服务端的错误(internal / unknown / unavailable / deadline_exceeded / data_loss,和 SLO 的口径一致)= Error;
// 客户端的错误(invalid_argument、not_found 等)只记 rpc.connect_rpc.error_code,不算 span 的错误。
func NewInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !Enabled() {
				return next(ctx, req)
			}
			ctx = WithRemote(ctx, req.Header().Get("traceparent"))
			proc := req.Spec().Procedure
			ctx, span := Start(ctx, strings.TrimPrefix(proc, "/"), trace.SpanKindServer)
			defer span.End()
			if span.IsRecording() {
				svc, method := splitProcedure(proc)
				span.SetAttributes(attribute.String("rpc.system", "connect_rpc"), attribute.String("rpc.service", svc), attribute.String("rpc.method", method))
			}
			res, err := next(ctx, req)
			if err != nil && span.IsRecording() {
				code := connect.CodeOf(err)
				span.SetAttributes(attribute.String("rpc.connect_rpc.error_code", code.String()))
				if ServerError(code) {
					span.RecordError(err)
					span.SetStatus(codes.Error, code.String())
				}
			}
			return res, err
		}
	}
}

// ServerError: 这个错误码是服务端的错误(SLO 的坏事件)。
func ServerError(c connect.Code) bool {
	switch c {
	case connect.CodeInternal, connect.CodeUnknown, connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeDataLoss:
		return true
	}
	return false
}

func splitProcedure(p string) (service, method string) {
	p = strings.TrimPrefix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}
