// Package tracing: 分布式追踪(OpenTelemetry)。生成的服务都经过 server.Run,所以都自动接上,不用改生成的代码。
//
// 开关和配置全是 OpenTelemetry 的标准环境变量(Setup 读):
//
//	OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT   设置了才打开(OTLP/HTTP,例 http://tempo:4318)
//	OTEL_SDK_DISABLED=true / OTEL_TRACES_EXPORTER=none                 强制关闭
//	OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG                      采样(默认 parentbased_always_on;
//	                                                                   例 parentbased_traceidratio + 0.1)
//	OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES                       资源(默认 service.name = SERVICE_NAME)
//	OTEL_EXPORTER_OTLP_PROTOCOL                                        只支持 http/protobuf(默认)
//	OTEL_BSP_* / OTEL_EXPORTER_OTLP_*_HEADERS / *_TIMEOUT 等           SDK 自己读
//
// 什么都没设置 = 关闭: Enabled() 是一个原子读,埋点处先判断它,关闭时不创建 span、不分配内存、行为和以前逐字节相同
// (消息的 traceparent 照旧原样传递,见 message.Traceparent)。tracing_off_test.go 检查关闭时的埋点 0 分配。
//
// 埋了什么(span 名):
//
//	server    <包>.<服务>/<方法>          Connect 的每个请求(SpanKindServer,接请求头 traceparent)
//	txn       txn                          一个写事务(txn.Executor.InTx;序列化失败的重来是事件 txn.retry)
//	pgx       <SQL 动词> <表>               每条 SQL(BEGIN / COMMIT 也是;只在有正在记录的父 span 时)
//	outbox    outbox.write <类型>           写 outbox(SpanKindProducer);envelope 的 traceparent = 这个 span
//	relay     publish <类型>                中继发布一条(父 = envelope 的 traceparent;AMQP header traceparent = 这个 span)
//	consume   process <队列>                处理一条消息(SpanKindConsumer,父 = AMQP header 的 traceparent)
//	sm        事件 sm.transition            状态机的转移(生成的处理器调用 sm.Transitioned);超时扫描每个实体一个 span sm.timeout <机器>
//	leader    leader <任务>                 当选 / 下台各一个短 span,事件 leader.acquired / leader.released / leader.lost
//
// 所以一个订单(PlaceOrder → order 的 outbox → inventory → payment → … → notification)是一条 trace。
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Scope: 运行时的 tracer 名(instrumentation scope)。
const Scope = "github.com/MIERAI/api-factory/runtime"

type state struct {
	tracer trace.Tracer
}

var cur atomic.Pointer[state]

// Enabled: 追踪打开了没有。埋点先判断它: 关闭时什么都不做。
func Enabled() bool { return cur.Load() != nil }

// noop: 关闭时 Start 返回的 span(包级的单例,不分配)。
var noop = trace.SpanFromContext(context.Background())

// Start: 打开时开始一个 span(ctx 带上它);关闭时原样返回 ctx 和一个什么都不做的 span(不分配)。
// 属性在调用处 `if span.IsRecording() { span.SetAttributes(…) }` 里加: 关闭 / 没被采样时不构造属性。
func Start(ctx context.Context, name string, kind trace.SpanKind) (context.Context, trace.Span) {
	s := cur.Load()
	if s == nil {
		return ctx, noop
	}
	return s.tracer.Start(ctx, name, trace.WithSpanKind(kind))
}

// StartRoot: 同 Start,但不接 ctx 里的父 span(新的 trace;后台任务: 超时扫描、选主)。
func StartRoot(ctx context.Context, name string, kind trace.SpanKind) (context.Context, trace.Span) {
	s := cur.Load()
	if s == nil {
		return ctx, noop
	}
	return s.tracer.Start(ctx, name, trace.WithSpanKind(kind), trace.WithNewRoot())
}

// Use: 用 tp 打开追踪(测试: 内存里的 exporter;嵌入运行时的程序自己配好的 provider)。返回恢复原状的函数。
// 不改 OpenTelemetry 的全局 provider。
func Use(tp trace.TracerProvider) (restore func()) {
	prev := cur.Swap(&state{tracer: tp.Tracer(Scope)})
	return func() { cur.Store(prev) }
}

// Config: Setup 的参数(环境变量没给时的默认值)。
type Config struct {
	ServiceName string // OTEL_SERVICE_NAME 没设置时的 service.name(server: SERVICE_NAME)
	Version     string // service.version(server: APP_VERSION)
	Log         *slog.Logger
}

// 环境变量(server.Env 列出;README 从那里生成;server 的 env_test 认 `const XxxEnv = "…"` 的写法)。
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"
const TracesEndpointEnv = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
const DisabledEnv = "OTEL_SDK_DISABLED"
const ExporterEnv = "OTEL_TRACES_EXPORTER"
const ProtocolEnv = "OTEL_EXPORTER_OTLP_PROTOCOL"
const SamplerEnv = "OTEL_TRACES_SAMPLER"
const SamplerArgEnv = "OTEL_TRACES_SAMPLER_ARG"
const ServiceNameEnv = "OTEL_SERVICE_NAME"
const ResourceEnv = "OTEL_RESOURCE_ATTRIBUTES"

// Wanted: 环境变量要不要打开追踪(没有 endpoint、被禁用 → false)。
func Wanted(getenv func(string) string) bool {
	if strings.EqualFold(strings.TrimSpace(getenv(DisabledEnv)), "true") {
		return false
	}
	if e := strings.TrimSpace(getenv(ExporterEnv)); e != "" && e != "otlp" {
		return false // none(或者不支持的,Setup 报错)
	}
	return getenv(EndpointEnv) != "" || getenv(TracesEndpointEnv) != ""
}

// Setup: 按环境变量打开追踪(没有 endpoint = 关闭,返回什么都不做的 shutdown)。配置写错了 → 错误(不带病启动)。
// shutdown 把缓冲里的 span 发完(上限 = ctx 的期限)。
func Setup(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	nop := func(context.Context) error { return nil }
	if e := strings.TrimSpace(os.Getenv(ExporterEnv)); e != "" && e != "otlp" && e != "none" {
		return nop, fmt.Errorf("%s=%q: only otlp or none", ExporterEnv, e)
	}
	if !Wanted(os.Getenv) {
		return nop, nil
	}
	if p := strings.TrimSpace(os.Getenv(ProtocolEnv)); p != "" && p != "http/protobuf" {
		return nop, fmt.Errorf("%s=%q: only http/protobuf is supported (OTLP over HTTP, port 4318)", ProtocolEnv, p)
	}
	sampler, err := SamplerFromEnv(os.Getenv)
	if err != nil {
		return nop, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	exp, err := otlptracehttp.New(ctx) // endpoint / headers / timeout: OTEL_EXPORTER_OTLP_*
	if err != nil {
		return nop, fmt.Errorf("tracing: OTLP exporter: %w", err)
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", cfg.ServiceName)}
	if cfg.Version != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Version))
	}
	// 后面的覆盖前面的: OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES 优先
	res, err := resource.New(ctx, resource.WithAttributes(attrs...), resource.WithTelemetrySDK(), resource.WithFromEnv())
	if err != nil {
		return nop, fmt.Errorf("tracing: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp), // OTEL_BSP_*: 队列满了丢 span,不阻塞请求
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetErrorHandler(newErrorHandler(log, time.Minute))
	otel.SetTracerProvider(tp)
	restore := Use(tp)
	log.Info("tracing on", "endpoint", firstNonEmpty(os.Getenv(TracesEndpointEnv), os.Getenv(EndpointEnv)),
		"sampler", firstNonEmpty(os.Getenv(SamplerEnv), "parentbased_always_on"), "sampler_arg", os.Getenv(SamplerArgEnv))
	return func(ctx context.Context) error {
		restore()
		return tp.Shutdown(ctx)
	}, nil
}

// SamplerFromEnv: OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG(规范的六种;默认 parentbased_always_on)。
// 写错了是错误(SDK 自己读时只会退回默认值,悄悄地全采样)。
func SamplerFromEnv(getenv func(string) string) (sdktrace.Sampler, error) {
	name := strings.ToLower(strings.TrimSpace(getenv(SamplerEnv)))
	arg := strings.TrimSpace(getenv(SamplerArgEnv))
	ratio := func() (float64, error) {
		if arg == "" {
			return 1, nil
		}
		r, err := strconv.ParseFloat(arg, 64)
		if err != nil || r < 0 || r > 1 {
			return 0, fmt.Errorf("%s=%q: a ratio between 0 and 1", SamplerArgEnv, arg)
		}
		return r, nil
	}
	switch name {
	case "", "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "traceidratio", "parentbased_traceidratio":
		r, err := ratio()
		if err != nil {
			return nil, err
		}
		if name == "traceidratio" {
			return sdktrace.TraceIDRatioBased(r), nil
		}
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(r)), nil
	}
	return nil, fmt.Errorf("%s=%q: one of always_on, always_off, traceidratio, parentbased_always_on, parentbased_always_off, parentbased_traceidratio", SamplerEnv, name)
}

// errorHandler: SDK 的错误(导出失败: 收集端连不上)写 WARN,每个间隔最多一条(其余的计数,下一条带上),免得刷屏。
type errorHandler struct {
	log   *slog.Logger
	every time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func newErrorHandler(log *slog.Logger, every time.Duration) *errorHandler {
	return &errorHandler{log: log, every: every}
}

func (h *errorHandler) Handle(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.last.IsZero() && time.Since(h.last) < h.every {
		h.suppressed++
		return
	}
	h.log.Warn("tracing: export failed (spans are dropped; requests are not affected)", "err", err, "suppressed", h.suppressed)
	h.last, h.suppressed = time.Now(), 0
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// Fail: span 标成错误(打开且在记录时)。
func Fail(span trace.Span, err error) {
	if err == nil || !span.IsRecording() {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
