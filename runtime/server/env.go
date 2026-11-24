package server

import (
	"github.com/MIERAI/api-factory/runtime/clock"
	"github.com/MIERAI/api-factory/runtime/outbox"
	"github.com/MIERAI/api-factory/runtime/tracing"
	"github.com/MIERAI/api-factory/runtime/txn"
)

// EnvVar: 生成的服务读的一个环境变量(G22: 项目的 README 从这份清单生成,不手抄)。
type EnvVar struct {
	Name    string
	Default string // 空 = 没有默认值
	// When: 什么时候需要 —— "required"(必须设置,否则不启动)/ "messaging"(服务发送或处理消息时必须设置)/
	// "optional" / "test"(只在测试环境设置;生产不要设)
	When string
	Desc string
}

// 需要的时机(EnvVar.When)。
const (
	EnvRequired  = "required"
	EnvMessaging = "messaging"
	EnvOptional  = "optional"
	EnvTest      = "test"
)

// Env: 运行时读的全部环境变量(server 和它启动的后台任务;env_test.go 检查运行时的源码里读的每个都在这里)。
var Env = []EnvVar{
	{"DATABASE_URL", "", EnvRequired, "Postgres connection string (the service's own database)"},
	{"AUTH_MODE", "", EnvRequired, "`token` (Bearer token checked by the server) or `none` (the platform authenticates)"},
	{"AUTH_TOKEN_SHA256", "", EnvOptional, "with AUTH_MODE=token (then required): SHA-256 hex of each accepted token, comma separated"},
	{"AMQP_URL", "", EnvMessaging, "RabbitMQ connection string with the environment's vhost. Without it the outbox is written but never sent, no handler consumes and no state machine times out (a WARN at start-up)"},
	{"PORT", "8080", EnvOptional, "the API and the probes /livez /readyz /healthz"},
	{"METRICS_PORT", "", EnvOptional, "/metrics without authentication on this port (the image sets 9090)"},
	{"SERVICE_NAME", "api-factory-service", EnvOptional, "the RabbitMQ connection name (the image sets it to the service's name)"},
	{"PAGE_TOKEN_KEY", "", EnvOptional, "key that signs page tokens; without it a random key per process (tokens break across restarts and instances; a WARN)"},
	{"FACTORY_SHARDS", "", EnvOptional, "shard routing for sharded tables, e.g. `1-2=postgres://…;*=postgres://…` (empty = everything in DATABASE_URL)"},
	{"REQUEST_TIMEOUT_MS", "3000", EnvOptional, "per-request timeout"},
	{"SHUTDOWN_TIMEOUT_MS", "15000", EnvOptional, "graceful shutdown limit for HTTP and for the background workers (each); keep it ≥ the message transaction timeout (10s)"},
	{"APP_VERSION", "dev", EnvOptional, "returned in /healthz as X-App-Version (the image sets it from the build arg VERSION)"},
	{tracing.EndpointEnv, "", EnvOptional, "OTLP/HTTP collector, e.g. `http://tempo:4318`: distributed tracing is on when this (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is set, off otherwise. The other standard OTEL_* variables apply (OTEL_BSP_*, OTEL_EXPORTER_OTLP_HEADERS / _TIMEOUT, …)"},
	{tracing.TracesEndpointEnv, "", EnvOptional, "the collector's full traces URL (`…/v1/traces`); overrides OTEL_EXPORTER_OTLP_ENDPOINT"},
	{tracing.ProtocolEnv, "http/protobuf", EnvOptional, "only `http/protobuf` (anything else: the server does not start)"},
	{tracing.SamplerEnv, "parentbased_always_on", EnvOptional, "head sampling: `parentbased_traceidratio` with OTEL_TRACES_SAMPLER_ARG keeps that share of new traces and follows the caller's decision (one order = one trace, sampled or not as a whole)"},
	{tracing.SamplerArgEnv, "1.0", EnvOptional, "the ratio of the *traceidratio samplers (0–1)"},
	{tracing.ServiceNameEnv, "", EnvOptional, "service.name of the spans (default: SERVICE_NAME)"},
	{tracing.ResourceEnv, "", EnvOptional, "more resource attributes, e.g. `k8s.namespace.name=shop-prod,deployment.environment.name=prod`"},
	{tracing.DisabledEnv, "", EnvOptional, "`true`: tracing off whatever else is set"},
	{tracing.ExporterEnv, "otlp", EnvOptional, "`none`: tracing off; only `otlp` otherwise"},
	{"INSTANCE_ID", "", EnvTest, "echoed by /healthz (tests: which process answered)"},
	{clock.TestClockEnv, "", EnvTest, "`1`: the request header X-Factory-Now sets now(); state machine timeouts are driven by the test entry"},
	{txn.TestFaultsEnv, "", EnvTest, "`1`: fault injection headers and the in-process message test entry are honoured"},
	{outbox.RelayCrashEnv, "", EnvTest, "with FACTORY_TEST_FAULTS=1: the relay exits at a crash point (crash tests)"},
}
