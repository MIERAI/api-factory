package perf

// Load test requests for write APIs (factory loadgen): the same request values as the perf write checks (write.go) —
// update / delete keys from existing rows, inserts with new keys from the data generator — written as the API's
// request (protojson). Skew (ParseSkew) decides how the existing rows are spread over the requests.

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/ir"
)

// ParseSkew: uniform (default: every request a different row while there are enough, then again from the start) /
// zipf:s (row k with probability ∝ 1/k^s: a few hot keys, a long tail) / hot:N (every request one of the first N rows).
// Returns the pick function of genRequests (nil = distinct rows) — deterministic (fixed seed).
func ParseSkew(s string) (func(n, i int) int, error) {
	kind, arg, _ := strings.Cut(s, ":")
	rng := rand.New(rand.NewPCG(1, 2))
	switch kind {
	case "", "uniform":
		return nil, nil
	case "hot":
		k, err := strconv.Atoi(arg)
		if err != nil || k < 1 {
			return nil, fmt.Errorf("--skew hot:N: N ≥ 1 (%q)", arg)
		}
		return func(n, _ int) int { return rng.IntN(min(k, n)) }, nil
	case "zipf":
		v, err := strconv.ParseFloat(arg, 64)
		if err != nil || v <= 1 {
			return nil, fmt.Errorf("--skew zipf:s: s > 1 (%q)", arg)
		}
		var z *rand.Zipf
		zn := -1
		return func(n, _ int) int {
			if n != zn { // the generator is per candidate count
				z, zn = rand.NewZipf(rng, v, 1, uint64(n-1)), n
			}
			return int(z.Uint64())
		}, nil
	}
	return nil, fmt.Errorf("--skew: uniform | zipf:s | hot:N (%q)", s)
}

// loadTables: the generator's view of a database filled by seed-volume / perf (row counts = the tables' sizes: new
// rows of the requests come after them).
func loadTables(ctx context.Context, conn *pgx.Conn, plan *ir.Plan) (map[string]*table, error) {
	tables, err := loadSchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := applyPlan(plan, tables); err != nil {
		return nil, err
	}
	for _, t := range tables {
		var n int64
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{t.name}.Sanitize()).Scan(&n); err != nil {
			return nil, err
		}
		t.n = max(int(n), 1)
	}
	return tables, nil
}

// writeRequests: n requests per kind of request of a write API.
func writeRequests(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, tables map[string]*table, n int, seq map[string]int, pick func(n, i int) int) ([]Request, error) {
	specs, err := writeSpecs(api)
	if err != nil {
		return nil, err
	}
	var out []Request
	for i, ws := range specs {
		envs, err := genRequests(ctx, conn, ws, tables, n, seq, pick)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ws.variant, err)
		}
		req, path, kind, typ := &api.Request, "/"+api.ProtoPackage+"."+api.Service+"/"+api.Method, "write", ""
		var eachField string
		for _, op := range ws.ops {
			if op.Each != "" {
				eachField = op.Each
			}
		}
		switch c := api.Command; {
		case c.Op == "message":
			req, path, kind, typ = &c.Receive.Message, "", "message", c.Receive.Type
			eachField = c.Each
		case c.Op == "machine":
			r := c.Machine.Receives[i]
			ct := c.Machine.Contract(r.Type)
			if ct == nil {
				return nil, fmt.Errorf("状态机: 没有 %s 的契约", r.Type)
			}
			req, path, kind, typ = &ct.Message, "", "message", r.Type
		}
		bind := bindInputs(ws.ops)
		for _, env := range envs {
			body, err := requestBody(plan, api, req, env, bind, eachField)
			if err != nil {
				return nil, err
			}
			out = append(out, Request{API: api.ID, Path: path, Body: body, Kind: kind, Type: typ, SingleUse: singleUse(ws.ops)})
		}
	}
	return out, nil
}

// singleUse: sending the request twice does not do the same work (a new key, a delete, an optimistic version).
func singleUse(ops []ir.CommandOp) bool {
	for _, op := range ops {
		if op.Kind == "insert" || op.Kind == "delete" || op.Lock != nil || op.Kind == "insert_idempotent" {
			return true
		}
	}
	return false
}

// requestBody: the request message (protojson) from a request's values; fields without a value take the spec's
// first OK request's (they still pass the request's validation).
func requestBody(plan *ir.Plan, api *ir.API, msg *ir.Message, env *reqEnv, bind map[string]binding, each string) (map[string]any, error) {
	body := map[string]any{}
	for _, f := range msg.Fields {
		if f.Name == each && f.Message != nil {
			var base map[string]any
			if xs, ok := api.BaseRequest[f.Name].([]any); ok && len(xs) > 0 {
				base, _ = xs[0].(map[string]any)
			}
			var list []any
			for _, it := range env.items {
				m := map[string]any{}
				for _, sf := range f.Message.Fields {
					if v, ok := it[sf.Name]; ok {
						j, err := protoValue(plan, &sf, v, bind["item:"+sf.Name])
						if err != nil {
							return nil, err
						}
						m[sf.Name] = j
					} else if b, ok := base[sf.Name]; ok {
						m[sf.Name] = b
					}
				}
				list = append(list, m)
			}
			body[f.Name] = list
			continue
		}
		if v, ok := env.inputs[f.Name]; ok {
			j, err := protoValue(plan, &f, v, bind["input:"+f.Name])
			if err != nil {
				return nil, err
			}
			body[f.Name] = j
		} else if b, ok := api.BaseRequest[f.Name]; ok {
			body[f.Name] = b
		}
	}
	return body, nil
}

// protoValue: a database value → the field's protojson value (enums: DB value → name; timestamps RFC 3339).
func protoValue(plan *ir.Plan, f *ir.Field, v any, b binding) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case time.Time:
		if f.Kind == "timestamp" {
			return x.UTC().Format(time.RFC3339Nano), nil
		}
		return x.Format("2006-01-02"), nil
	}
	if f.Kind == "enum" {
		db, err := strconv.ParseInt(fmt.Sprint(v), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f.Name, err)
		}
		if en := plan.Enum(b.table, b.col); en != nil {
			for _, e := range en.Values {
				if e.DB == db {
					return e.Name, nil
				}
			}
		}
		for _, e := range f.Values {
			if int64(e.Number) == db {
				return e.Name, nil
			}
		}
		return nil, fmt.Errorf("%s: no enum value for %d", f.Name, db)
	}
	if fl, ok := v.(float64); ok && fl == math.Trunc(fl) {
		return int64(fl), nil
	}
	return v, nil
}

