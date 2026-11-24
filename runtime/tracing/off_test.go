package tracing_test

// 关闭时(默认)埋点的开销可以忽略: 每个埋点处只是一个原子读,不创建 span、不分配内存。
// 这里查分配(确定、不受机器快慢影响);每个埋点的耗时和一次提交相比的比例在性能闸门里量(outbox/perf_gate_test.go ⑤)。

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/sm"
	"github.com/MIERAI/api-factory/runtime/tracing"
)

func TestOffHooksDoNotAllocate(t *testing.T) {
	if tracing.Enabled() {
		t.Fatal("tracing is on by default")
	}
	ctx := context.Background()
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	withTP := message.WithTraceparent(ctx, tp)
	for name, f := range map[string]func(){
		"Start+End": func() {
			_, span := tracing.Start(ctx, "x", trace.SpanKindInternal)
			if span.IsRecording() {
				t.Fatal("recording while off")
			}
			span.End()
		},
		"StartRoot+End": func() {
			_, span := tracing.StartRoot(ctx, "x", trace.SpanKindInternal)
			span.End()
		},
		"message.Traceparent": func() {
			if message.Traceparent(withTP) != tp {
				t.Fatal("traceparent lost while off")
			}
		},
		"Enabled": func() { _ = tracing.Enabled() },
	} {
		if n := testing.AllocsPerRun(1000, f); n != 0 {
			t.Errorf("%s allocates %.1f times per call with tracing off", name, n)
		}
	}
	// 状态机的转移: 新的(带 ctx)和以前的分配一样多
	old := testing.AllocsPerRun(1000, func() { sm.Transition("M", "A", "B", "t") })
	cur := testing.AllocsPerRun(1000, func() { sm.Transitioned(ctx, "M", "A", "B", "t") })
	if cur != old {
		t.Errorf("sm.Transitioned allocates %.1f, sm.Transition %.1f", cur, old)
	}
}
