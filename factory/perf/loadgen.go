package perf

// LoadRequests: realistic requests for load tests (soak / step load). For every API, rows sampled from the real data
// fill its filters, so equality / prefix filters hit existing rows; range filters get the smaller / larger value of
// two rows (from < to). The rest of the request (and APIs without declarative filters) comes from BaseRequest, the
// first OK request of the spec. Valid-at and tuple filters keep BaseRequest's values.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

type Request struct {
	API  string         `json:"api"`
	Path string         `json:"path"` // Connect: /<package>.<Service>/<Method>
	Body map[string]any `json:"body"`
}

func LoadRequests(ctx context.Context, plan *ir.Plan, db string, perAPI int) ([]Request, error) {
	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	var out []Request
	for i := range plan.APIs {
		api := &plan.APIs[i]
		rs, err := apiRequests(ctx, conn, plan, api, perAPI)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", api.ID, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

func apiRequests(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, n int) ([]Request, error) {
	path := "/" + api.ProtoPackage + "." + api.Service + "/" + api.Method
	base := func() map[string]any {
		m := map[string]any{}
		for k, v := range api.BaseRequest {
			m[k] = v
		}
		return m
	}
	var fs []ir.Filter
	for _, f := range api.Filters {
		switch f.Op {
		case "eq", "ne", "in", "prefix", "contains", "gt", "gte", "lt", "lte":
			if api.Request.Field(f.Field) != nil {
				fs = append(fs, f)
			}
		}
	}
	var out []Request
	if api.Pipeline == nil || len(fs) == 0 {
		for range n {
			out = append(out, Request{API: api.ID, Path: path, Body: base()})
		}
		return out, nil
	}
	// one query: n*2 random rows with every filter column (as text, timestamps in RFC 3339)
	var exprs, conds []string
	for _, f := range fs {
		k, ok := kindOfSQL(f.ColumnType)
		if !ok {
			k = core.Text
		}
		e := columnExpr(plan, api, f.Column, k)
		conds = append(conds, e+" IS NOT NULL")
		if k == core.Timestamp {
			e = "to_char(" + e + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
		}
		exprs = append(exprs, e+"::text")
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY random() LIMIT %d", strings.Join(exprs, ", "),
		pgx.Identifier{api.Pipeline.Steps[0].Table}.Sanitize(), strings.Join(conds, " AND "), n*2)
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, q)
	}
	var sample [][]string
	for rows.Next() {
		r := make([]string, len(fs))
		dst := make([]any, len(fs))
		for i := range r {
			dst[i] = &r[i]
		}
		if err := rows.Scan(dst...); err != nil {
			rows.Close()
			return nil, err
		}
		sample = append(sample, r)
	}
	rows.Close()
	if len(sample) == 0 { // empty table: the base request only
		for range n {
			out = append(out, Request{API: api.ID, Path: path, Body: base()})
		}
		return out, nil
	}
	for i := range n {
		a, b := sample[i%len(sample)], sample[rand.IntN(len(sample))]
		body := base()
		for j, f := range fs {
			fld := api.Request.Field(f.Field)
			lo, hi := a[j], b[j]
			if less(hi, lo, f.ColumnType) {
				lo, hi = hi, lo
			}
			var v string
			switch f.Op {
			case "eq", "ne":
				v = a[j]
			case "prefix", "contains":
				v = string([]rune(a[j])[:min(2, len([]rune(a[j])))])
			case "gt", "gte":
				v = lo
			case "lt", "lte":
				v = hi
				if k, ok := kindOfSQL(f.ColumnType); ok && (k == core.Date || k == core.Timestamp) {
					v = plusDays(lo, 7, k) // a week: within "at most N days" rules, like a real screen's period
				}
				if v == lo { // from == to would be empty (or invalid): keep the base request's bound
					continue
				}
			case "in":
				x, err := jsonValue(plan, api, f, fld, a[j])
				if err != nil {
					return nil, err
				}
				y, err := jsonValue(plan, api, f, fld, b[j])
				if err != nil {
					return nil, err
				}
				body[f.Field] = []any{x, y}
				continue
			}
			x, err := jsonValue(plan, api, f, fld, v)
			if err != nil {
				return nil, err
			}
			if fld.List {
				x = []any{x}
			}
			body[f.Field] = x
		}
		out = append(out, Request{API: api.ID, Path: path, Body: body})
	}
	return out, nil
}

// columnExpr: a logical column of the API's table (JSON-versioned storage: the JSON key, cast).
func columnExpr(plan *ir.Plan, api *ir.API, col string, k core.Kind) string {
	expr := pgx.Identifier{col}.Sanitize()
	if st := plan.Storage(api.Table); st != nil {
		for _, f := range st.Fields {
			if f.Name == col && f.Column != "" {
				expr = pgx.Identifier{f.Column}.Sanitize()
			} else if f.Name == col {
				expr = fmt.Sprintf("(%s ->> %s)::%s", pgx.Identifier{st.DataColumn}.Sanitize(), quote(f.Key), k.SQLType())
			}
		}
	}
	return expr
}

// plusDays: an ISO date / RFC 3339 timestamp + d days (unparsable: unchanged).
func plusDays(s string, d int, k core.Kind) string {
	layout := "2006-01-02"
	if k == core.Timestamp {
		layout = time.RFC3339
	}
	t, err := time.Parse(layout, s)
	if err != nil {
		return s
	}
	return t.AddDate(0, 0, d).Format(layout)
}

func less(a, b, sqlType string) bool {
	if k, ok := kindOfSQL(sqlType); ok && (k == core.Int32 || k == core.Int64 || k == core.Decimal) {
		x, e1 := strconv.ParseFloat(a, 64)
		y, e2 := strconv.ParseFloat(b, 64)
		if e1 == nil && e2 == nil {
			return x < y
		}
	}
	return a < b // ISO dates / timestamps, text
}

// jsonValue: a database value (text) as the request field's protojson value (enums: DB value → name, plan.Enums).
func jsonValue(plan *ir.Plan, api *ir.API, f ir.Filter, fld *ir.Field, s string) (any, error) {
	switch fld.Kind {
	case "int32", "int64":
		n, err := strconv.ParseInt(strings.SplitN(s, ".", 2)[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", fld.Name, s, err)
		}
		return n, nil
	case "enum":
		db, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", fld.Name, s, err)
		}
		if en := plan.Enum(api.Table, f.Column); en != nil {
			for _, v := range en.Values {
				if v.DB == db {
					return v.Name, nil
				}
			}
		}
		return nil, fmt.Errorf("%s: no enum value for %d", fld.Name, db)
	}
	return s, nil // string (text, dates, decimals), timestamp (RFC 3339)
}
