package core

// now: 请求开始时的时刻(timestamp,不为 NULL)。值不来自请求: 服务器的时钟(测试时固定,见 NowParam)。

import (
	"fmt"
	"time"
)

// NowParam: now() 的值在参考求值器的 Params 里的键,也是 SQL 里的参数名(sqlc.arg(factory_now))。
// 适配器把它接到运行时的时钟(请求开始的时刻);请求(proto)里没有这个字段。
const NowParam = "factory_now"

func Now() *Expr { return &Expr{Op: "now"} }

func init() {
	registerExpr(exprPrim{
		op:      "now",
		request: true,
		typeOf: func(_ *Pipeline, e *Expr, _ Schema) (Type, error) {
			if len(e.Args) != 0 {
				return Type{}, fmt.Errorf("now 没有参数")
			}
			return Type{Kind: Timestamp}, nil
		},
		eval: func(_ *Pipeline, _ *Expr, _ Row, _ Schema, params Params) (any, error) {
			t, ok := params[NowParam].(time.Time)
			if !ok {
				return nil, fmt.Errorf("now(): 参数 %s 没有给(测试要固定请求开始的时刻)", NowParam)
			}
			return t.UTC(), nil
		},
		sql: func(g *sqlGen, _ *Expr, _ *sqlLevel, _ bool) (string, error) {
			s, _ := g.param(NowParam)
			return s, nil
		},
	})
}
