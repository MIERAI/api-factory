package tracing

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// PGX: pgx 的 QueryTracer —— 每条 SQL 一个 span。名字见 Summary: 生成的查询带 "-- name: X :kind" 注释 → "X"
// (例 "GetProductLabel");其余 = "<动词> <表>"(例 "INSERT factory_outbox"、"COMMIT")。
// 只在 ctx 里有正在记录的 span 时才建(请求、事务、消息的子 span);后台的轮询(中继、选主、超时扫描的查询)没有父 span,
// 不产生成千上万条孤立的 trace。server 在追踪打开时把它装到连接池上(ConnConfig.Tracer)。
type PGX struct{}

type pgxSpanKey struct{}

// MaxQueryText: db.query.text 的上限(SQL 只有占位符,不含参数值)。
const MaxQueryText = 2048

func (PGX) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}
	ctx, span := Start(ctx, Summary(data.SQL), trace.SpanKindClient)
	if span.IsRecording() {
		q := data.SQL
		if len(q) > MaxQueryText {
			q = q[:MaxQueryText]
		}
		attrs := []attribute.KeyValue{attribute.String("db.system.name", "postgresql"), attribute.String("db.query.text", q)}
		if conn != nil {
			attrs = append(attrs, attribute.String("db.namespace", conn.Config().Database))
		}
		span.SetAttributes(attrs...)
	}
	return context.WithValue(ctx, pgxSpanKey{}, span)
}

func (PGX) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(pgxSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		Fail(span, data.Err)
	} else if span.IsRecording() {
		span.SetAttributes(attribute.Int64("db.response.affected_rows", data.CommandTag.RowsAffected()))
	}
	span.End()
}

// Summary: span 名。先跳过开头的空白和注释(-- 行注释、/* */ 块注释,可嵌套);开头注释里有
// "-- name: X :kind"(工厂 / sqlc 生成的查询都带)就用 X。否则 = SQL 的第一个词(大写)+ 主要的表
// (INTO / UPDATE / FROM 之后的第一个名字)。
func Summary(sql string) string {
	rest, name := stripLeadingComments(sql)
	if name != "" {
		return name
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "SQL"
	}
	verb := strings.ToUpper(f[0])
	var after string
	switch verb {
	case "INSERT":
		after = "INTO"
	case "UPDATE":
		if len(f) > 1 {
			return verb + " " + trimName(f[1])
		}
		return verb
	case "DELETE", "SELECT":
		after = "FROM"
	default:
		return verb
	}
	for i := 1; i+1 < len(f); i++ {
		if strings.EqualFold(f[i], after) && !strings.HasPrefix(f[i+1], "(") {
			return verb + " " + trimName(f[i+1])
		}
	}
	return verb
}

// stripLeadingComments: 去掉开头的空白和注释,返回剩下的 SQL 和第一个 "-- name: X" 注释里的 X(没有 = "")。
// 没闭合的块注释 = 整条都是注释。
func stripLeadingComments(sql string) (rest, name string) {
	s := sql
	for {
		s = strings.TrimLeft(s, " \t\r\n\f\v")
		switch {
		case strings.HasPrefix(s, "--"):
			line := s[2:]
			if i := strings.IndexByte(line, '\n'); i >= 0 {
				s = line[i+1:]
				line = line[:i]
			} else {
				s = ""
			}
			if name == "" {
				name = queryName(line)
			}
		case strings.HasPrefix(s, "/*"):
			depth, i := 1, 2
			for i < len(s) && depth > 0 {
				switch {
				case strings.HasPrefix(s[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(s[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			s = s[i:]
		default:
			return s, name
		}
	}
}

// queryName: 行注释正文 " name: GetProductLabel :one" → "GetProductLabel";不是这个形式 → ""。
func queryName(comment string) string {
	f := strings.Fields(comment)
	if len(f) < 2 || f[0] != "name:" {
		return ""
	}
	return f[1]
}

func trimName(s string) string {
	if i := strings.IndexAny(s, "( ,;"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, `"`)
}
