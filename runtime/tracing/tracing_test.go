package tracing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestWanted(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{EndpointEnv: "http://t:4318"}, true},
		{map[string]string{TracesEndpointEnv: "http://t:4318/v1/traces"}, true},
		{map[string]string{EndpointEnv: "http://t:4318", DisabledEnv: "true"}, false},
		{map[string]string{EndpointEnv: "http://t:4318", DisabledEnv: "TRUE"}, false},
		{map[string]string{EndpointEnv: "http://t:4318", ExporterEnv: "none"}, false},
		{map[string]string{EndpointEnv: "http://t:4318", ExporterEnv: "otlp"}, true},
		{map[string]string{SamplerEnv: "always_on"}, false}, // 没有 endpoint = 关闭
	} {
		if got := Wanted(env(c.env)); got != c.want {
			t.Errorf("Wanted(%v) = %v", c.env, got)
		}
	}
}

func TestSetupOffWithoutEndpoint(t *testing.T) {
	for _, k := range []string{EndpointEnv, TracesEndpointEnv, DisabledEnv, ExporterEnv, ProtocolEnv, SamplerEnv, SamplerArgEnv} {
		t.Setenv(k, "")
	}
	shutdown, err := Setup(context.Background(), Config{ServiceName: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("tracing on without an endpoint")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSetupRejectsBadConfig(t *testing.T) {
	for _, c := range []map[string]string{
		{EndpointEnv: "http://127.0.0.1:1", ProtocolEnv: "grpc"},
		{EndpointEnv: "http://127.0.0.1:1", SamplerEnv: "sometimes"},
		{EndpointEnv: "http://127.0.0.1:1", SamplerEnv: "parentbased_traceidratio", SamplerArgEnv: "1.5"},
		{ExporterEnv: "zipkin"},
	} {
		for _, k := range []string{EndpointEnv, TracesEndpointEnv, DisabledEnv, ExporterEnv, ProtocolEnv, SamplerEnv, SamplerArgEnv} {
			t.Setenv(k, c[k])
		}
		if _, err := Setup(context.Background(), Config{ServiceName: "x"}); err == nil {
			t.Errorf("%v: no error", c)
		}
		if Enabled() {
			t.Fatalf("%v: tracing on after a configuration error", c)
		}
	}
}

// Setup 打开之后: span 经 OTLP/HTTP 发到 endpoint,service.name / 资源属性来自环境变量。
func TestSetupExportsOTLP(t *testing.T) {
	var got atomic.Int64
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(r.Body)
			body.Store(b.String())
			got.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv(EndpointEnv, srv.URL)
	t.Setenv(TracesEndpointEnv, "")
	t.Setenv(DisabledEnv, "")
	t.Setenv(ExporterEnv, "")
	t.Setenv(ProtocolEnv, "")
	t.Setenv(SamplerEnv, "parentbased_traceidratio")
	t.Setenv(SamplerArgEnv, "1")
	t.Setenv(ResourceEnv, "k8s.namespace.name=v2-test")
	t.Setenv(ServiceNameEnv, "")
	shutdown, err := Setup(context.Background(), Config{ServiceName: "order", Version: "abc", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if !Enabled() {
		t.Fatal("not enabled")
	}
	_, span := Start(context.Background(), "hello", trace.SpanKindInternal)
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("still enabled after shutdown")
	}
	if got.Load() == 0 {
		t.Fatal("no OTLP export")
	}
	b, _ := body.Load().(string)
	for _, want := range []string{"hello", "order", "v2-test", "abc"} {
		if !strings.Contains(b, want) {
			t.Errorf("export does not contain %q", want)
		}
	}
}

func TestSamplerFromEnv(t *testing.T) {
	for name, want := range map[string]string{
		"":                         "ParentBased{root:AlwaysOnSampler",
		"always_on":                "AlwaysOnSampler",
		"always_off":               "AlwaysOffSampler",
		"traceidratio":             "TraceIDRatioBased{0.25}",
		"parentbased_traceidratio": "ParentBased{root:TraceIDRatioBased{0.25}",
		"parentbased_always_off":   "ParentBased{root:AlwaysOffSampler",
	} {
		s, err := SamplerFromEnv(env(map[string]string{SamplerEnv: name, SamplerArgEnv: "0.25"}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(s.Description(), want) {
			t.Errorf("%s: %s, want %s…", name, s.Description(), want)
		}
	}
	for _, arg := range []string{"x", "-0.1", "2"} {
		if _, err := SamplerFromEnv(env(map[string]string{SamplerEnv: "traceidratio", SamplerArgEnv: arg})); err == nil {
			t.Errorf("arg %q accepted", arg)
		}
	}
}

func TestFormatParse(t *testing.T) {
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc := Parse(tp)
	if !sc.IsValid() || !sc.IsRemote() || !sc.IsSampled() {
		t.Fatalf("Parse: %+v", sc)
	}
	if got := Format(sc); got != tp {
		t.Fatalf("Format = %s", got)
	}
	if got := Format(Parse("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")); !strings.HasSuffix(got, "-00") {
		t.Fatalf("unsampled flags lost: %s", got)
	}
	for _, bad := range []string{"", "garbage", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e473x-00f067aa0ba902b7-01"} {
		if Parse(bad).IsValid() {
			t.Errorf("Parse(%q) valid", bad)
		}
	}
	ctx := WithRemote(context.Background(), tp)
	if Current(ctx) != tp || TraceID(ctx) != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("Current %s TraceID %s", Current(ctx), TraceID(ctx))
	}
	if WithRemote(context.Background(), "bad") != context.Background() {
		t.Fatal("bad traceparent changed ctx")
	}
}

func TestSummary(t *testing.T) {
	for sql, want := range map[string]string{
		`INSERT INTO factory_outbox (id, kind) VALUES ($1, $2)`:                            "INSERT factory_outbox",
		`insert into "orders"("id") values ($1)`:                                           "INSERT orders",
		`UPDATE "inventory_items" SET "available" = "available" - $1 WHERE "sku" = $2`:     "UPDATE inventory_items",
		`SELECT seq, id::text FROM factory_outbox WHERE published_at IS NULL`:              "SELECT factory_outbox",
		`SELECT pg_try_advisory_lock($1)`:                                                  "SELECT",
		`DELETE FROM factory_outbox WHERE seq IN (SELECT seq FROM factory_outbox LIMIT 1)`: "DELETE factory_outbox",
		`SELECT x FROM (SELECT 1) s`:                                                       "SELECT",
		"begin isolation level read committed":                                             "BEGIN",
		"commit":                                                                           "COMMIT",
		"":                                                                                 "SQL",
	} {
		if got := Summary(sql); got != want {
			t.Errorf("Summary(%q) = %q, want %q", sql, got, want)
		}
	}
}

// 生成的查询以 "-- name: X :kind" 开头(factory/adapters/golang/querygen.go);以前 span 名取了第一个词 = "--"。
func TestSummarySkipsLeadingComments(t *testing.T) {
	for sql, want := range map[string]string{
		"-- name: GetProductLabel :one\nSELECT t3.text AS text\nFROM (SELECT 1) AS t3\n":             "GetProductLabel",
		"\n  -- name: ListTopSales :many\r\nSELECT a FROM sales":                                     "ListTopSales",
		"-- Code generated by factory. DO NOT EDIT.\n\n-- name: AdjustStock :exec\nUPDATE x SET a=1": "AdjustStock",
		"/* name: NotALineComment */ SELECT a FROM t":                                                "SELECT t",
		"-- just a note\nINSERT INTO factory_outbox (id) VALUES ($1)":                                "INSERT factory_outbox",
		"/* a */ /* b /* nested */ still b */\n\tDELETE FROM factory_outbox":                         "DELETE factory_outbox",
		"-- name:\nCOMMIT":                    "COMMIT",
		"--name: X :one\nSELECT 1":            "X",
		"-- only a comment":                   "SQL",
		"/* unterminated SELECT a FROM t":     "SQL",
		"   \n\t ":                            "SQL",
		"SELECT a -- name: Late :one\nFROM t": "SELECT t",
	} {
		if got := Summary(sql); got != want {
			t.Errorf("Summary(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestErrorHandlerRateLimited(t *testing.T) {
	var buf bytes.Buffer
	h := newErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)), time.Hour)
	for i := 0; i < 50; i++ {
		h.Handle(errors.New("connection refused"))
	}
	if n := strings.Count(buf.String(), "export failed"); n != 1 {
		t.Fatalf("%d log lines for 50 errors, want 1", n)
	}
	if h.suppressed != 49 {
		t.Fatalf("suppressed %d", h.suppressed)
	}
}

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	t.Cleanup(Use(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))))
	return rec
}

// 拦截器: 父 = 请求头 traceparent;服务端的错误 = Error,客户端的错误不是。
func TestInterceptor(t *testing.T) {
	rec := recorder(t)
	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	call := func(err error) sdktrace.ReadOnlySpan {
		t.Helper()
		var inner string
		h := NewInterceptor()(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			inner = Current(ctx)
			return nil, err
		})
		req := connect.NewRequest(&emptypb.Empty{})
		req.Header().Set("traceparent", parent)
		_, _ = h(context.Background(), req)
		spans := rec.Ended()
		s := spans[len(spans)-1]
		if s.SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || s.Parent().SpanID().String() != "00f067aa0ba902b7" {
			t.Fatalf("span not a child of the request's traceparent: %s parent %s", s.SpanContext().TraceID(), s.Parent().SpanID())
		}
		if s.SpanKind() != trace.SpanKindServer {
			t.Fatalf("kind %v", s.SpanKind())
		}
		if inner != Format(s.SpanContext()) {
			t.Fatalf("handler ctx %s, span %s", inner, Format(s.SpanContext()))
		}
		return s
	}
	if s := call(nil); s.Status().Code == codes.Error {
		t.Error("ok call marked as error")
	}
	if s := call(connect.NewError(connect.CodeNotFound, errors.New("no"))); s.Status().Code == codes.Error {
		t.Error("not_found marked as a span error")
	}
	if s := call(connect.NewError(connect.CodeUnavailable, errors.New("db down"))); s.Status().Code != codes.Error {
		t.Error("unavailable not marked as error")
	}
}

// PGX: 没有正在记录的父 span → 不建 span、不碰 ctx;有 → 子 span,结束时带错误。
func TestPGXTracerOnlyUnderAParent(t *testing.T) {
	rec := recorder(t)
	var tr PGX
	ctx := context.Background()
	if got := tr.TraceQueryStart(ctx, nil, pgxStart("SELECT 1")); got != ctx {
		t.Fatal("a span without a parent")
	}
	tr.TraceQueryEnd(ctx, nil, pgxEnd(nil))
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("%d spans without a parent", n)
	}
	pctx, parent := Start(ctx, "parent", trace.SpanKindInternal)
	qctx := tr.TraceQueryStart(pctx, nil, pgxStart(`UPDATE "t" SET a = 1`))
	tr.TraceQueryEnd(qctx, nil, pgxEnd(errors.New("deadlock")))
	parent.End()
	spans := rec.Ended()
	if len(spans) != 2 || spans[0].Name() != "UPDATE t" || spans[0].Parent().SpanID() != parent.SpanContext().SpanID() || spans[0].Status().Code != codes.Error {
		t.Fatalf("spans: %v", spans)
	}
}

func pgxStart(sql string) pgx.TraceQueryStartData { return pgx.TraceQueryStartData{SQL: sql} }
func pgxEnd(err error) pgx.TraceQueryEndData      { return pgx.TraceQueryEndData{Err: err} }
