package tracing

import (
	"context"
	"encoding/hex"

	"go.opentelemetry.io/otel/trace"
)

// W3C traceparent(只用 version 00): "00-<32 位 trace id>-<16 位 span id>-<2 位 flags>"。
// HTTP 请求头、消息的 envelope 和 AMQP header 用的都是它。

// Format: span context → traceparent(不合法 → "")。
func Format(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	var b [55]byte
	b[0], b[1], b[2] = '0', '0', '-'
	tid, sid := sc.TraceID(), sc.SpanID()
	hex.Encode(b[3:35], tid[:])
	b[35] = '-'
	hex.Encode(b[36:52], sid[:])
	b[52] = '-'
	hex.Encode(b[53:55], []byte{byte(sc.TraceFlags())})
	return string(b[:])
}

// Parse: traceparent → 远端的 span context(不合法 → 无效的零值)。
func Parse(tp string) trace.SpanContext {
	if len(tp) != 55 || tp[:3] != "00-" || tp[35] != '-' || tp[52] != '-' {
		return trace.SpanContext{}
	}
	tid, err := trace.TraceIDFromHex(tp[3:35])
	if err != nil {
		return trace.SpanContext{}
	}
	sid, err := trace.SpanIDFromHex(tp[36:52])
	if err != nil {
		return trace.SpanContext{}
	}
	var f [1]byte
	if _, err := hex.Decode(f[:], []byte(tp[53:55])); err != nil {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.TraceFlags(f[0]) & trace.FlagsSampled, Remote: true})
}

// WithRemote: ctx 带上 traceparent 表示的远端父 span(不合法 / 空 → ctx 不变)。
func WithRemote(ctx context.Context, tp string) context.Context {
	if sc := Parse(tp); sc.IsValid() {
		return trace.ContextWithRemoteSpanContext(ctx, sc)
	}
	return ctx
}

// Current: ctx 里当前 span 的 traceparent(没有 → "")。
func Current(ctx context.Context) string {
	return Format(trace.SpanContextFromContext(ctx))
}

// TraceID: ctx 里当前 span 的 trace id(没有 → "";日志用,Loki → Tempo 的链接)。
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
