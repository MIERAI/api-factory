package compiler

// 负载与热点(docs/design/hotspots.md): 共享数量的读改写的判断、平台默认的负载、同时抢: 是 → 分桶的生产线
// (形状不支持 → 静态拒绝)。分桶的 DDL = db/schema/zz_factory_buckets.sql;写 / 读的表名换成视图(IR 的 WriteTo /
// 流水线的 scan),模型和验收仍按原来的表。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"apifactory/factory/command"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// BucketFile: 分桶的 DDL(factory prepare 生成;幂等,走项目的迁移流程)。
var BucketFile = filepath.Join("db", "schema", "zz_factory_buckets.sql")

// DefaultBuckets: 桶数(⚖️ H5)。
const DefaultBuckets = 16

// defaultLoad: 平台按 API 种类的默认(⚖️ H7;和 slo-platform 的默认 SLO 一致)。
func defaultLoad(api *ir.API) ir.LoadSpec {
	switch {
	case api.Command == nil:
		return ir.LoadSpec{QPS: 50, P99Ms: 300, Skew: "uniform"}
	case len(api.Shared) > 0 && api.Hot == "yes":
		return ir.LoadSpec{QPS: 200, P99Ms: 500, Skew: "hot:1"}
	}
	return ir.LoadSpec{QPS: 20, P99Ms: 500, Skew: "uniform"}
}

func resolveLoad(api *ir.API, d *spec.LoadDecl) *ir.LoadSpec {
	l := defaultLoad(api)
	if d != nil {
		l.Declared = true
		if d.QPS > 0 {
			l.QPS = d.QPS
		}
		if d.P99 != "" {
			fmt.Sscanf(d.P99, "%dms", &l.P99Ms)
		}
		if d.Skew != "" {
			l.Skew = d.Skew
		}
	}
	return &l
}

// hotIR: 每个 API 的 Shared / Hot / Load;同时抢: 是 的表 → 分桶(检查形状、改表名、写 DDL)。
// final: 查询文件(读 API 的表名换成视图之后重写)。
func hotIR(specs []*spec.Spec, apis []ir.API, cat *catalog, keys map[string][][]string, final map[string]string) ([]ir.Bucket, error) {
	type hotCol struct{ table, col string }
	yes := map[hotCol][]string{}
	buckets := map[hotCol]int{}
	var errs []string
	for i, s := range specs {
		a := &apis[i]
		if a.Command != nil && a.Command.Op != "machine" {
			a.Shared = command.SharedQuantity(command.OpsOf(a.Command))
		}
		hot := s.API.Hot
		switch {
		case hot != "" && len(a.Shared) == 0:
			errs = append(errs, fmt.Sprintf("%s: 同时抢 只对共享数量的读改写有意义(按键更新里 列: \"列 ± …\"),这个 API 没有;删掉这一行", s.Path))
		case hot == "" && len(a.Shared) > 0:
			hot = "unsure" // 没回答 = 不确定(⚖️ H2: 闸门不失败;普通的生产线 + 热键监控)
		}
		a.Hot = hot
		a.Load = resolveLoad(a, s.API.Load)
		if hot == "yes" {
			for _, q := range a.Shared {
				k := hotCol{q.Table, q.Column}
				yes[k] = append(yes[k], a.ID)
				if s.API.Load != nil && s.API.Load.Buckets > 0 {
					buckets[k] = max(buckets[k], s.API.Load.Buckets)
				}
			}
		}
	}
	var out []ir.Bucket
	hcs := make([]hotCol, 0, len(yes))
	for k := range yes {
		hcs = append(hcs, k)
	}
	sort.Slice(hcs, func(i, j int) bool { return hcs[i].table+"."+hcs[i].col < hcs[j].table+"."+hcs[j].col })
	byTable := map[string]bool{}
	for _, k := range hcs {
		b, why := bucketShape(k.table, k.col, specs, apis, cat, keys)
		if why != "" {
			errs = append(errs, fmt.Sprintf("同时抢: 是(%s)—— 表 %s 的 %s 不能分桶: %s。\n"+
				"  热点的一行会让同时的请求排队(上限 ≈ 1 / 一次事务的时间,make perf 的单键探针给出实测)。\n"+
				"  改法: 把这个数量单独放在一张只有 主键 + 数量 的表里,只用「%s: \"%s - 量\"」+「条件: \"%s >= 量\"」取、「%s + 量」还;\n"+
				"  或者回答 同时抢: 否 / 不确定(普通的一行 + 热键监控)",
				strings.Join(yes[k], ", "), k.table, k.col, why, k.col, k.col, k.col, k.col))
			continue
		}
		if byTable[k.table] {
			errs = append(errs, fmt.Sprintf("同时抢: 是 —— 表 %s 有两个数量列都要分桶(%s 之外还有 %s),现在只支持一个", k.table, b.Column, k.col))
			continue
		}
		byTable[k.table] = true
		b.N = DefaultBuckets
		if n := buckets[k]; n > 0 {
			b.N = n
		}
		b.APIs = yes[k]
		out = append(out, b)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	// 改表名: 写(更新 / 删除)→ 视图;读(流水线的 scan / 关联的对方)→ 视图
	for _, b := range out {
		for i := range apis {
			a := &apis[i]
			if c := a.Command; c != nil {
				if c.Table == b.Table && (c.Op == "update" || c.Op == "delete") {
					c.WriteTo = b.View
				}
				for j := range c.Ops {
					if op := &c.Ops[j]; op.Table == b.Table && (op.Kind == "update" || op.Kind == "delete") {
						op.WriteTo = b.View
					}
				}
			}
			if a.Pipeline != nil && renameTable(a.Pipeline.Steps, b.Table, b.View) {
				q, err := core.ToSQL(a.Pipeline, core.Options{ParamRef: core.Sqlc})
				if err != nil {
					return nil, fmt.Errorf("%s: %w", a.SpecPath, err)
				}
				a.Query.SQL = q.Text
				final[specs[i].ID()] = queryFile(specs[i], q.Text)
			}
		}
	}
	return out, writeBucketSQL(out, cat)
}

// renameTable: 流水线(含关联 / 嵌套 / 子表合计的对方)里读表 from 的地方换成 to。
func renameTable(steps []core.Step, from, to string) bool {
	changed := false
	for i := range steps {
		st := &steps[i]
		switch st.Kind {
		case "scan", "lookup", "nest", "child_agg":
			if st.Table == from && len(st.Source) == 0 {
				st.Table, changed = to, true
			}
		}
		if renameTable(st.Source, from, to) {
			changed = true
		}
	}
	return changed
}

// bucketShape: 分桶支持的形状(docs/design/hotspots.md §4.1)。不支持 → 原因。
func bucketShape(table, col string, specs []*spec.Spec, apis []ir.API, cat *catalog, keys map[string][][]string) (ir.Bucket, string) {
	b := ir.Bucket{Table: table, Column: col, View: "factory_hot_" + table, BucketTable: "factory_bucket_" + table}
	cols, ok := cat.tables[table]
	if !ok {
		return b, "表不在 db/schema 里"
	}
	for _, c := range cols {
		if c.Name == col {
			b.Type = c.Type
		}
	}
	switch b.Type {
	case "int2", "int4", "int8":
	default:
		return b, fmt.Sprintf("数量列的类型是 %s(只支持整数)", b.Type)
	}
	if ks := keys[table]; len(ks) > 0 {
		b.Key = ks[0]
	} else {
		return b, "表没有主键"
	}
	per := map[string]bool{}
	sameKey := func(op ir.CommandOp) bool {
		if len(op.Key) != len(b.Key) {
			return false
		}
		for i, k := range op.Key {
			if k.Column != b.Key[i] {
				return false
			}
		}
		return true
	}
	for i := range apis {
		a := &apis[i]
		if a.Command == nil {
			if a.Query.Escape && strings.Contains(a.Query.SQL, table) {
				return b, fmt.Sprintf("%s 是逃生口的 SQL,直接读这张表(读不到桶的合计)", a.ID)
			}
			continue
		}
		if a.Command.IsMachine() {
			if a.Command.Machine != nil && a.Command.Machine.Table == table {
				return b, fmt.Sprintf("%s 是这张表的状态机", a.ID)
			}
			continue
		}
		for _, op := range command.OpsOf(a.Command) {
			if op.Table != table {
				continue
			}
			switch op.Kind {
			case "insert_idempotent":
				return b, fmt.Sprintf("%s 的 %s 是幂等新建(ON CONFLICT 的时候表的触发器已经改了桶)", a.ID, op.Name)
			case "insert":
				for _, r := range op.Returning {
					if r.Name == col {
						return b, fmt.Sprintf("%s 的 %s 新建之后要返回 %s(新建写进表本身,数量在桶里)", a.ID, op.Name, col)
					}
				}
				continue
			case "delete":
				continue
			}
			var delta *ir.CommandVal
			for j, v := range op.Values {
				if v.Column == col {
					if _, ok := command.SelfDelta(v); !ok {
						return b, fmt.Sprintf("%s 的 %s 把 %s 设成一个值(只支持 %s ± 量)", a.ID, op.Name, col, col)
					}
					delta = &op.Values[j]
				}
			}
			if delta == nil {
				continue // 不改数量的更新: 写表本身(冷路径)
			}
			if !sameKey(op) {
				return b, fmt.Sprintf("%s 的 %s 不是按主键 %v 更新", a.ID, op.Name, b.Key)
			}
			if op.Lock != nil {
				return b, fmt.Sprintf("%s 的 %s 有乐观锁(版本列每次都变 = 每个请求都改同一行,分桶也没用)", a.ID, op.Name)
			}
			for _, v := range op.Values {
				switch {
				case v.Column == col:
				case v.Expr != nil && v.Expr.Op == "now":
					per[v.Column] = true
				default:
					return b, fmt.Sprintf("%s 的 %s 同时改 %s(每个请求都改同一行的其他列,分桶也没用;只能另外设 现在)", a.ID, op.Name, v.Column)
				}
			}
			if sign, _ := command.SelfDelta(*delta); sign == "-" && !condIsAtLeast(op.Cond, col, command.Delta(*delta)) {
				return b, fmt.Sprintf("%s 的 %s 取的时候要写 条件: \"%s >= 量\"(和扣的量相同;不能超卖的规则)", a.ID, op.Name, col)
			}
		}
	}
	for c := range per {
		b.PerBucket = append(b.PerBucket, c)
	}
	sort.Strings(b.PerBucket)
	return b, ""
}

// condIsAtLeast: 条件正好是 col >= x。
func condIsAtLeast(c *ir.CmdExpr, col string, x *ir.CmdExpr) bool {
	if c == nil || c.Op != ">=" || len(c.Args) != 2 || c.Args[0].Op != "col" || c.Args[0].Name != col {
		return false
	}
	a, _ := json.Marshal(stripType(c.Args[1]))
	b, _ := json.Marshal(stripType(x))
	return string(a) == string(b)
}

func stripType(x *ir.CmdExpr) *ir.CmdExpr {
	if x == nil {
		return nil
	}
	c := *x
	c.Type = ""
	c.Args = nil
	for _, a := range x.Args {
		c.Args = append(c.Args, stripType(a))
	}
	return &c
}

const bucketHead = `-- Code generated by factory (factory prepare). DO NOT EDIT.
-- 分桶的生产线(同时抢: 是;docs/design/hotspots.md §4): 数量列拆成 N 个桶,写 / 读经过视图 factory_hot_<表>。
-- 幂等: 对已有的库执行 = 建桶表 + 视图 + 触发器 + 把现在的数量分到桶里。迁移 = 这个文件的内容 + Down。
`

// writeBucketSQL: 分桶的 DDL。没有 → 删掉文件。
func writeBucketSQL(bs []ir.Bucket, cat *catalog) error {
	if len(bs) == 0 {
		if err := os.Remove(BucketFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var b strings.Builder
	b.WriteString(bucketHead)
	for _, bk := range bs {
		var cols []core.BucketCol
		for _, c := range cat.tables[bk.Table] {
			cols = append(cols, core.BucketCol{Name: c.Name, Type: c.Type})
		}
		b.WriteString("\n" + core.BucketDDL(core.BucketSpec{Table: bk.Table, Column: bk.Column, Type: bk.Type, Key: bk.Key, View: bk.View,
			BucketTable: bk.BucketTable, N: bk.N, PerBucket: bk.PerBucket, Cols: cols}))
	}
	if old, err := os.ReadFile(BucketFile); err == nil && string(old) == b.String() {
		return nil
	}
	return os.WriteFile(BucketFile, []byte(b.String()), 0o644)
}
