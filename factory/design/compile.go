// Package design 是设计编译器: 中文式样书(design/<领域>/)→ 机器用的 proto + spec.yaml + _domain.yaml。
//
// 确定性的一对一翻译,不猜: 写不清楚的地方直接报错(中文,带文件和行号)。
// 人批准的是中文式样书,所以这里绝不能"理解"自由文本——说明、业务规则只原样带下去。
package design

import (
	"apifactory/factory/proj"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"apifactory/factory/adapter"
	"apifactory/factory/cells"
	"apifactory/factory/compiler"
	"apifactory/factory/constraints"
	"apifactory/factory/core"
	"apifactory/factory/kinds"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
	"apifactory/factory/where"
)

type Config struct {
	PackagePrefix string // factory
	Version       string // v1
}

func LoadConfig() (Config, error) {
	b, err := proj.Config()
	if err != nil {
		return Config{}, err
	}
	var f struct {
		Proto struct {
			PackagePrefix string `yaml:"package_prefix"`
			Version       string `yaml:"version"`
		} `yaml:"proto"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return Config{}, err
	}
	if f.Proto.PackagePrefix == "" || f.Proto.Version == "" {
		return Config{}, fmt.Errorf("factory.yaml: proto.package_prefix / proto.version are required")
	}
	return Config{PackagePrefix: f.Proto.PackagePrefix, Version: f.Proto.Version}, nil
}

// 编译一个领域需要的上下文
type ctx struct {
	lib     *cells.Library
	cfg     Config
	d       *Domain
	cat     map[string][]compiler.Column
	tz      *time.Location
	lock    *Lock
	pkg     string // factory.jobs.v1
	service string // JobService
	msgs    []*message
	msgByNm map[string]*message
}

type message struct {
	Name   string
	Desc   string
	Fields []*field
	CEL    []cel
}

type field struct {
	Name     string
	Type     string // proto 类型: int64 / string / JobStatus / google.protobuf.Timestamp / Job
	Optional bool
	Repeated bool
	Desc     string
	Rules    []string // buf.validate 的字段规则(一行一个)
	kind     string   // 值转换用: int / float / string / bool / time / enum:<名字> / message:<名字>
}

type cel struct{ ID, Message, Expr string }

// CompileDir: design/<领域>/ → proto + specs/<领域>/...(生成)。返回生成的文件。
func CompileDir(dir string, m *adapter.Manifest) ([]string, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	d, apis, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	cat, err := compiler.Catalog()
	if err != nil {
		return nil, err
	}
	// 存储: JSON版本 的逻辑表: 列来自领域声明(之后当普通的表用)
	for name, t := range d.Tables {
		if t.Storage == nil {
			continue
		}
		src, ok := sources.ByKind(t.Storage.Kind) // 逻辑表的列由来源决定
		if !ok || src.Columns == nil {
			return nil, fmt.Errorf("表 %s: 存储 %s 不存在", name, t.Storage.Kind)
		}
		var cols []compiler.Column
		for _, c := range src.Columns(t.Storage, t.PrimaryKey) {
			cols = append(cols, compiler.Column{Name: c.Name, Type: c.SQLType, NotNull: c.NotNull})
		}
		cat[name] = cols
	}
	for name, t := range d.Tables { // 编码(整数里的日期): 列必须是整数
		for col := range t.ColEncode {
			ok := false
			for _, x := range cat[name] {
				ok = ok || (x.Name == col && (x.Type == "int4" || x.Type == "int8" || x.Type == "integer" || x.Type == "bigint"))
			}
			if !ok {
				return nil, fmt.Errorf("%s/领域.yaml: 表 %s 的列 %s: 编码 %s 只能用于整数的列", dir, name, col, EncodingYMD)
			}
		}
	}
	tz, err := time.LoadLocation(d.TZ)
	if err != nil {
		return nil, fmt.Errorf("%s/领域.yaml: 时区 %q 无法识别: %w", dir, d.TZ, err)
	}
	lock, err := loadLock(filepath.Join(dir, "proto.lock.yaml"))
	if err != nil {
		return nil, err
	}
	lib, err := cells.Load()
	if err != nil {
		return nil, err
	}
	c := &ctx{lib: lib, cfg: cfg, d: d, cat: cat, tz: tz, lock: lock, msgByNm: map[string]*message{},
		pkg: cfg.PackagePrefix + "." + d.Name + "." + cfg.Version, service: pascal(singular(d.Name)) + "Service"}

	var specs []*spec.Spec
	var rpcs []string
	for _, a := range apis {
		s, rpc, err := c.compileAPI(a)
		if err != nil {
			return nil, err
		}
		specs = append(specs, s)
		rpcs = append(rpcs, rpc)
	}

	protoDir := filepath.Join("proto", cfg.PackagePrefix, d.Name, cfg.Version)
	protoFile := filepath.Join(protoDir, d.Name+".proto")
	vars := adapter.Vars{Domain: d.Name, ProtoFile: filepath.Join(cfg.PackagePrefix, d.Name, cfg.Version, d.Name+".proto")}
	src := c.renderProto(rpcs, m.ProtoFileOptions(vars))
	var written []string
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(protoFile, []byte(src), 0o644); err != nil {
		return nil, err
	}
	written = append(written, protoFile)

	specDir := filepath.Join("specs", d.Name)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		return nil, err
	}
	// 只删除以前生成的式样书(人写的旧格式不动)
	old, _ := filepath.Glob(filepath.Join(specDir, "*", "spec.yaml"))
	for _, f := range old {
		if b, _ := os.ReadFile(f); strings.HasPrefix(string(b), generatedMark) {
			_ = os.Remove(f)
		}
	}
	for i, s := range specs {
		path := filepath.Join(specDir, kebab(apis[i].Name), "spec.yaml")
		if err := writeYAML(path, apis[i].Path, s); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	dpath := filepath.Join(specDir, "_domain.yaml")
	if err := writeYAML(dpath, filepath.Join(dir, "领域.yaml"), c.machineDomain()); err != nil {
		return nil, err
	}
	written = append(written, dpath)
	if err := lock.save(); err != nil {
		return nil, err
	}
	return written, nil
}

const generatedMark = "# Code generated by factory"

func writeYAML(path, from string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("%s from %s. DO NOT EDIT.\n# 人写的式样书在 %s(这个文件由设计编译器生成)\n", generatedMark, from, from)
	return os.WriteFile(path, append([]byte(head), b...), 0o644)
}

// ---------------------------------------------------------------------------
// API → message + spec
// ---------------------------------------------------------------------------

func (c *ctx) column(table, col string) (compiler.Column, bool) {
	for _, x := range c.cat[table] {
		if x.Name == col {
			return x, true
		}
	}
	return compiler.Column{}, false
}

// typeOf: 中文类型名(或枚举名)→ proto 类型 + 值的种类
func (c *ctx) typeOf(t string) (string, string, bool) {
	if k, ok := kinds.ByName(t); ok { // 类型的注册表(factory/kinds)
		return k.Proto, k.Value, true
	}
	if c.d.Enum(t) != nil {
		return t, "enum:" + t, true
	}
	return "", "", false
}

// columnField: 输出字段的类型从数据来源的列推导(枚举看领域层)
func (c *ctx) columnField(table, col string) (string, string, bool, error) {
	cc, ok := c.column(table, col)
	if !ok {
		return "", "", false, fmt.Errorf("表 %s 没有列 %s", table, col)
	}
	if t := c.d.Tables[table]; t != nil {
		if en := t.ColEnum[col]; en != "" {
			return en, "enum:" + en, false, nil
		}
	}
	k, ok := kinds.BySQL(cc.Type)
	p := [2]string{}
	if ok {
		p = [2]string{k.Proto, k.Value}
	}
	if !ok {
		return "", "", false, fmt.Errorf("表 %s 的列 %s: 类型 %s 还不支持", table, col, cc.Type)
	}
	optional := !cc.NotNull && p[1] != "time" // Timestamp 本身就能表示"没有值"
	return p[0], p[1], optional, nil
}

// groupedField: 分组之后的输出字段。分组键 = 表的列(枚举看领域层);计算 / 排名 = 核心决定的类型。
func (c *ctx) groupedField(table, col string, groupedCol func(string) (compiler.Column, bool), g *spec.GroupBy) (string, string, bool, error) {
	gc, ok := groupedCol(col)
	if !ok {
		return "", "", false, fmt.Errorf("分组之后只有 分组键 / 计算 / 排名 的列,没有 %s", col)
	}
	if contains(g.Keys, col) {
		return c.columnField(table, col)
	}
	k, ok := kinds.BySQL(gc.Type)
	if !ok {
		return "", "", false, fmt.Errorf("类型 %s 还不支持", gc.Type)
	}
	return k.Proto, k.Value, !gc.NotNull && k.Value != "time", nil
}

func (c *ctx) inputField(a *API, in *Input, e *errs) *field {
	if in.Type == GroupListType { // 组的列表: repeated <API><组名>,组里的字段都必须指定
		name := a.Name + pascal(singular(in.Name))
		m := &message{Name: name, Desc: in.Desc}
		for _, mi := range in.Group {
			if strings.HasSuffix(mi.Type, "列表") {
				e.add(mi.Line, "输入 %s 的组: 字段 %s 不能是列表", in.Name, mi.Name)
				continue
			}
			if d := c.d.Enum(mi.Type); d != nil {
				e.add(mi.Line, "输入 %s 的组: 字段 %s 还不能是枚举", in.Name, mi.Name)
				continue
			}
			if mf := c.inputField(a, mi, e); mf != nil {
				m.Fields = append(m.Fields, mf)
			}
		}
		if len(m.Fields) < 2 {
			e.add(in.Line, "输入 %s: 组至少要有 2 个字段(1 个字段请用 列表 + 属于)", in.Name)
		}
		c.addMessage(m)
		f := &field{Name: in.Name, Type: name, Repeated: true, Desc: in.Desc, kind: "message:" + name}
		for k := range in.Constraints {
			if k != "个数" {
				e.add(in.Line, "输入 %s: 组列表 只能写 个数(其他约束写在组的字段上)", in.Name)
			}
		}
		rules, cerrs := constraints.Compile(constraints.Ctx{ProtoType: name, List: true, Required: in.Required}, in.Constraints)
		for _, err := range cerrs {
			e.add(in.Line, "输入 %s: %v", in.Name, err)
		}
		if in.Required && !constraints.Has(in.Constraints, "个数") {
			f.Rules = append(f.Rules, "repeated.min_items = 1")
		}
		f.Rules = append(f.Rules, rules...)
		if in.Desc != "" && !in.Required {
			f.Desc = in.Desc + "(空列表 = 不按这个条件筛选)"
		}
		return f
	}
	elemType, isList := strings.CutSuffix(in.Type, "列表")
	pt, kind, ok := c.typeOf(elemType)
	if !ok {
		e.add(in.Line, "输入 %s: 类型 %q 无法识别(%s / 领域里定义的枚举名,后面加「列表」表示多个)", in.Name, in.Type, kinds.Names())
		return nil
	}
	f := &field{Name: in.Name, Type: pt, Desc: in.Desc, kind: kind}
	typ, _ := kinds.ByName(elemType)
	if in.Encoding != "" { // 整数编码的日期: 不合法的日期 → 参数错误(proto 的 CEL;核心的 ymd 认为合法的范围)
		switch {
		case in.Encoding != EncodingYMD:
			e.add(in.Line, "输入 %s: 编码 只能是 %s", in.Name, EncodingYMD)
		case isList || typ == nil || (typ.Core != core.Int32 && typ.Core != core.Int64):
			e.add(in.Line, "输入 %s: 编码 %s 只能用于 整数32 / 整数64 的输入(不是列表)", in.Name, EncodingYMD)
		default:
			f.Rules = append(f.Rules, fmt.Sprintf("cel = {id: \"%s.ymd\", message: \"%s must be a valid date (YYYYMMDD)\", expression: \"%s\"}", in.Name, in.Name, ymdCEL))
		}
	}
	// 约束(范围 / 长度 / 个数 …)由注册表编译(factory/constraints;列表时元素级的约束用于每个元素)
	rules, cerrs := constraints.Compile(constraints.Ctx{
		ProtoType: pt, List: isList, Required: in.Required, HasPattern: typ != nil && typ.Pattern != "",
	}, in.Constraints)
	for _, err := range cerrs {
		e.add(in.Line, "输入 %s: %v", in.Name, err)
	}
	if isList {
		f.Repeated = true
		if in.Required && !constraints.Has(in.Constraints, "个数") { // 个数 会接管 min_items
			f.Rules = append(f.Rules, "repeated.min_items = 1")
		}
		f.Rules = append(f.Rules, rules...)
		if in.Desc != "" && !in.Required {
			f.Desc = in.Desc + "(空列表 = 不按这个条件筛选)"
		}
		return f
	}
	switch {
	case strings.HasPrefix(kind, "enum:"):
		f.Optional = !in.Required
		f.Rules = append(f.Rules, "enum = {defined_only: true, not_in: [0]}")
	case kind == "time":
		if in.Required {
			f.Rules = append(f.Rules, "required = true")
		}
	case typ != nil && typ.Pattern != "": // 格式由 protovalidate 检查(日期 / 十进制 …)
		f.Optional = !in.Required
		f.Rules = append(f.Rules, fmt.Sprintf("string.pattern = \"%s\"", typ.ProtoPattern()))
	default:
		f.Optional = !in.Required
	}
	f.Rules = append(f.Rules, rules...)
	return f
}

func (c *ctx) compileAPI(a *API) (*spec.Spec, string, error) {
	e := &errs{file: a.Path}
	table := a.Source
	if table != "" {
		if _, ok := c.cat[table]; !ok {
			e.add(1, "数据来源 %q 不是数据库里的表", table)
		}
	}

	checkPickUniquePlain(c, a, table, e)

	// ---- 关联: 加的列之后和来源的列一样(筛选 / 输出 / 来自 / 固定条件 / 排序)。例子的准备数据仍然按表的真实的列
	lookups := c.lookups(a, table, e)
	nests, childAggs := c.children(a, table, e) // 嵌套 / 子聚合: 加的列同样进目录(嵌套的列只能输出)
	rawCols := c.cat[table]
	if len(lookups)+len(nests)+len(childAggs) > 0 && e.err() == nil {
		view, err := compiler.LookupColumns(&spec.Spec{Domain: c.machineDomain(),
			Datasource: spec.Datasource{Table: table, Lookups: lookups, Nests: nests, ChildAggs: childAggs}}, c.cat)
		if err != nil {
			line := 1
			switch {
			case len(a.Lookups) > 0:
				line = a.Lookups[0].Line
			case len(a.Nests) > 0:
				line = a.Nests[0].Line
			case len(a.ChildAggs) > 0:
				line = a.ChildAggs[0].Line
			}
			e.add(line, "%v", err)
		} else {
			c.cat[table] = view
			defer func() { c.cat[table] = rawCols }()
		}
	}
	// 分组: 之后的列 = 分组键 + 计算 + 排名(输出 / 排序 / 例子的结果都看这些列)
	var grouped []compiler.Column
	if a.Pattern.Group != nil && table != "" {
		var err error
		if grouped, err = compiler.GroupedColumns(table, c.cat[table], a.Pattern.Group, a.Pattern.Rank); err != nil {
			e.add(1, "%v", err)
		}
		if t := c.d.Tables[table]; t != nil {
			for _, ag := range a.Pattern.Group.Aggs {
				if en := t.ColEnum[ag.Column]; en != "" {
					e.add(1, "分组.计算.%s: 列 %s 是枚举 %s,不能做 合计 / 最小 / 最大(枚举的数据库值没有大小的意义)", ag.Name, ag.Column, en)
				}
			}
		}
		if grouped != nil {
			seen := map[string]bool{}
			for _, gc := range grouped {
				if seen[gc.Name] {
					e.add(1, "分组: 输出名 %s 重复(分组键 / 计算 / 排名的输出名不能相同)", gc.Name)
				}
				seen[gc.Name] = true
			}
		}
	}
	groupedCol := func(name string) (compiler.Column, bool) {
		for _, gc := range grouped {
			if gc.Name == name {
				return gc, true
			}
		}
		return compiler.Column{}, false
	}

	// ---- 请求
	req := &message{Name: a.Name + "Request", Desc: a.Desc}
	if a.Exit.Request != nil { // 出口加到请求里的字段(分页: page_size / page_token)
		for _, x := range a.Exit.Request(a.Pattern) {
			req.Fields = append(req.Fields, &field{Name: x.Name, Type: x.Type, kind: x.Kind, Desc: x.Desc, Rules: x.Rules})
		}
	}
	filters := map[string]spec.Filter{}
	for _, in := range a.Inputs {
		if f := c.inputField(a, in, e); f != nil {
			req.Fields = append(req.Fields, f)
		}
		if in.Type == GroupListType {
			op, ok := c.lib.FilterID(strings.TrimSpace(in.Filter))
			if !ok || op != "in_tuples" {
				e.add(in.Line, "输入 %s: 组列表 的筛选写成 \"组属于\"(组的字段名 = 列名)", in.Name)
				continue
			}
			var cols []string
			for _, mi := range in.Group {
				if _, has := c.column(table, mi.Name); !has {
					e.add(mi.Line, "输入 %s 的组: 字段 %s 不是表 %s 的列(组的字段名 = 列名)", in.Name, mi.Name, table)
				}
				cols = append(cols, mi.Name)
			}
			if len(cols) > 0 {
				filters[in.Name] = spec.Filter{Column: cols[0], Columns: cols, Op: op}
			}
			continue
		}
		if in.Filter != "" && !a.Exit.Command { // 命令的输入: commandBindings(写入 / 键 / 乐观锁)
			parts := strings.Fields(in.Filter)
			op, ok := "", false
			if len(parts) == 2 { // spec / IR 里用单元的内部名(factory/cells: 等于 → eq)
				op, ok = c.lib.FilterID(parts[1])
			}
			if !ok {
				e.add(in.Line, "输入 %s: 筛选 %q 无法识别。写成 \"列 <筛选单元>\",单元库里有: %s", in.Name, in.Filter, strings.Join(c.lib.FilterNames(), " / "))
				continue
			}
			if op == "in_tuples" {
				e.add(in.Line, "输入 %s: 组属于 只能用于 类型 %s 的输入", in.Name, GroupListType)
				continue
			}
			cols := strings.Split(parts[0], "~") // 多列的单元: 开始列~结束列
			// 分组之后的筛选(HAVING 一类): 列是计算 / 名次的输出名(分组键不算: 分组键的筛选放在分组之前,结果一样,索引能用)
			grouped := false
			if a.Pattern.Group != nil && len(cols) == 1 {
				_, isOut := groupedCol(cols[0])
				isKey := false
				for _, k := range a.Pattern.Group.Keys {
					isKey = isKey || k == cols[0]
				}
				if isOut && !isKey {
					if _, src := c.column(table, cols[0]); src {
						e.add(in.Line, "输入 %s: 筛选的列 %s 既是表 %s 的列,又是分组之后的输出名(计算 / 排名),分不清筛选放在分组之前还是之后。请给计算 / 排名的输出改名", in.Name, cols[0], table)
						continue
					}
					grouped = true
				}
			}
			for _, col := range cols {
				if _, has := c.column(table, col); !has && !grouped {
					e.add(in.Line, "输入 %s: 筛选的列 %s 不在表 %s 里(分组之后的筛选: 写计算 / 排名的输出名)", in.Name, col, table)
				}
			}
			sf := spec.Filter{Column: cols[0], Op: op, Grouped: grouped}
			if len(cols) > 1 {
				sf.Columns = cols
			}
			if op == "prefix" || op == "contains" { // 前缀 / 一部分不是完整的值: 带格式的类型(邮编 …)永远通不过格式校验
				if k, ok := kinds.ByName(strings.TrimSuffix(in.Type, "列表")); ok && k.Pattern != "" {
					e.add(in.Line, "输入 %s: %s的输入不能是带格式的类型 %s(不是完整的%s,会被格式校验拒绝),请用 文本", in.Name, c.lib.FilterName(op), k.Name, k.Name)
				}
			}
			filters[in.Name] = sf
		}
	}
	c.commandBindings(a, table, e) // 命令: 写入 / 键 / 乐观锁 / 设置(查询的种类: 不能写这些键)
	// 输入之间的规则: 关系的注册表(factory/constraints/rel_*.go)。id = 关系名.参数…(验收从 IR 读回,自动生成边界用例)
	for _, r := range a.InputRules {
		id, x, err := constraints.CompileRelation(r, func(name string) (constraints.RelField, error) {
			for _, f := range req.Fields {
				if f.Name == name {
					required := false
					for _, in := range a.Inputs {
						if in.Name == name {
							required = in.Required
						}
					}
					return constraints.RelField{Name: f.Name, Kind: f.kind, List: f.Repeated, Optional: f.Optional, Required: required}, nil
				}
			}
			return constraints.RelField{}, fmt.Errorf("%s 不是输入", name)
		})
		if err != nil {
			e.add(1, "输入之间的规则 %q: %v", r, err)
			continue
		}
		req.CEL = append(req.CEL, cel{ID: id, Message: x.Message, Expr: x.Expr})
	}

	// ---- 响应
	resp := &message{Name: a.Name + "Response", Desc: a.Desc}
	rename := map[string]string{} // 输出改名: 响应字段 → 来源的列(来自)。计算的输出由 AI 的代码决定,不能改名
	for _, o := range a.Outputs {
		if o.From == "" || o.From == o.Name || a.Kind == "command" { // 组合命令: 来自 = 操作名.列(composite.go)
			continue
		}
		switch {
		case a.Pattern.Group != nil:
			e.add(o.Line, "输出 %s: 分组之后不能写 来自(输出的名字 = 分组键 / 计算 / 排名的输出)", o.Name)
		case a.Exit.AI:
			e.add(o.Line, "输出 %s: 来自 只能用于数据来源的列(%s 的输出由 AI 的代码决定)", o.Name, a.Exit.Name)
		case table == "":
			e.add(o.Line, "输出 %s: 来自 需要 数据来源", o.Name)
		default:
			if _, ok := c.column(table, o.From); !ok {
				e.add(o.Line, "输出 %s: 来自 %s 不是 %s 的列", o.Name, o.From, table)
			} else {
				rename[o.Name] = o.From
			}
		}
	}
	outFields := func(src string) []*field {
		var out []*field
		for _, o := range a.Outputs {
			f := &field{Name: o.Name, Desc: o.Desc}
			col := o.Name
			if r, ok := rename[o.Name]; ok && src != "" {
				col = r
			}
			if o.Type == "列表" {
				itemName := a.ItemName
				if itemName == "" {
					itemName = pascal(singular(o.Name))
				}
				item := &message{Name: itemName, Desc: o.Desc}
				for _, r := range o.Rows {
					pt, kind, ok := c.typeOf(r.Type)
					if !ok {
						e.add(r.Line, "输出 %s 的每行 %s: 类型 %q 无法识别", o.Name, r.Name, r.Type)
						continue
					}
					item.Fields = append(item.Fields, &field{Name: r.Name, Desc: r.Desc, Type: pt, kind: kind, Optional: r.Optional && kind != "time"})
				}
				c.addMessage(item)
				f.Type, f.kind, f.Repeated = item.Name, "message:"+item.Name, true
				out = append(out, f)
				continue
			}
			if n := nestNamed(a, col); n != nil && src != "" && o.Type == "" { // 嵌套: repeated 子行的消息
				item, err := c.nestItem(n)
				if err != nil {
					e.add(o.Line, "输出 %s: %v", o.Name, err)
					continue
				}
				f.Type, f.kind, f.Repeated = item.Name, "message:"+item.Name, true
				out = append(out, f)
				continue
			}
			if o.Type != "" {
				pt, kind, ok := c.typeOf(o.Type)
				if !ok {
					e.add(o.Line, "输出 %s: 类型 %q 无法识别", o.Name, o.Type)
					continue
				}
				f.Type, f.kind, f.Optional = pt, kind, o.Optional && kind != "time"
			} else {
				if src == "" {
					e.add(o.Line, "输出 %s: 计算的输出必须写 类型", o.Name)
					continue
				}
				if a.Pattern.Group != nil {
					pt, kind, opt, err := c.groupedField(src, col, groupedCol, a.Pattern.Group)
					if err != nil {
						e.add(o.Line, "输出 %s: %v", o.Name, err)
						continue
					}
					f.Type, f.kind, f.Optional = pt, kind, opt
					out = append(out, f)
					continue
				}
				pt, kind, opt, err := c.columnField(src, col)
				if err != nil {
					e.add(o.Line, "输出 %s: %v(没有同名列时请写 类型)", o.Name, err)
					continue
				}
				f.Type, f.kind, f.Optional = pt, kind, opt
			}
			out = append(out, f)
		}
		return out
	}
	if a.Exit.NotNullOrder { // 分页的游标: 排序列不能为空(在设计稿的层面报错,不等到编译器的设计规则检查)
		for _, col := range a.Pattern.OrderBy {
			if r, ok := rename[col]; ok { // 排序用响应字段名;可空性看来源的列
				col = r
			}
			cc, ok := c.column(table, col)
			if a.Pattern.Group != nil {
				cc, ok = groupedCol(col)
			}
			if ok && !cc.NotNull {
				e.add(1, "排序的列 %s 可能为空,%s 的排序列必须不为空(翻页的位置记在排序列上)。"+
					"请改用主键或不为空的列排序;存储: JSON版本 的字段除了主键都可能为空", col, a.Exit.Name)
			}
		}
	}
	itemKey := ""
	switch {
	case a.Kind == "command": // 组合命令: 响应字段 ← 操作的返回值(composite.go)
		resp.Fields = c.compositeFields(a, e)
	case a.Exit.Rows: // 响应 = 行的列表(+ 出口另外的字段)
		itemName := a.ItemName
		if itemName == "" {
			itemName = pascal(singular(table))
		}
		listField := a.ListField
		if listField == "" {
			listField = table
		}
		item := &message{Name: itemName, Fields: outFields(table)}
		if t := c.d.Tables[table]; t != nil {
			item.Desc = t.Desc
		}
		if prev, ok := c.msgByNm[item.Name]; ok {
			if !sameFields(prev, item) {
				e.add(1, "输出行 %s 已经被别的一览用过,但字段不同(请用 输出行名 改名)", item.Name)
			}
		} else {
			c.addMessage(item)
		}
		resp.Fields = []*field{{Name: listField, Type: item.Name, Repeated: true, kind: "message:" + item.Name}}
		if a.Exit.RowsDesc != nil {
			resp.Fields[0].Desc = a.Exit.RowsDesc(a.Pattern)
		}
		for _, x := range a.Exit.Extra {
			resp.Fields = append(resp.Fields, &field{Name: x.Name, Type: x.Type, kind: x.Kind, Desc: x.Desc, Rules: x.Rules})
		}
		if a.Exit.ItemKey != nil {
			itemKey = a.Exit.ItemKey(a.Pattern)
		}
	case a.Exit.FromTable: // 响应的字段 = 数据来源的列
		resp.Fields = outFields(table)
		if a.Exit.ExtraFor != nil {
			for _, x := range a.Exit.ExtraFor(a.Pattern) {
				for _, f := range resp.Fields {
					if f.Name == x.Name {
						e.add(1, "输出 %s 和出口加的字段重名(%s)", x.Name, a.Exit.Name)
					}
				}
				resp.Fields = append(resp.Fields, &field{Name: x.Name, Type: x.Type, kind: x.Kind, Desc: x.Desc, Rules: x.Rules})
			}
		}
	default: // 响应的字段 = 设计稿的 输出(计算)
		resp.Fields = outFields("")
	}
	if err := e.err(); err != nil {
		return nil, "", err
	}
	c.addMessage(req)
	c.addMessage(resp)

	// ---- 机器用的式样书
	s := &spec.Spec{SpecVersion: spec.Version}
	s.API = spec.API{RPC: c.pkg + "." + c.service + "/" + a.Name, Owner: a.Owner, RiskLevel: a.Risk, Description: a.Background}
	s.Pattern = a.Pattern
	s.Pattern.Kind = a.Kind
	s.Datasource = spec.Datasource{Table: table, Filters: filters, Where: a.Where, Lookups: lookups, Nests: nests, ChildAggs: childAggs}
	derived, usesNow := c.compileDerived(a, table, e)
	usesNow = usesNow || a.Pattern.Command.UsesNow() // 命令: 设置 用到 现在
	if len(derived) > 0 {
		if a.Exit.AI && a.Kind == "custom" {
			e.add(1, "派生参数 不能用于 %s(查询由 AI 写)", a.Exit.Name)
		}
		s.Datasource.Derived = derived
	}
	timeOpt := where.Options{Time: true, Zone: c.d.TZ, YMD: func(col string) bool {
		t := c.d.Tables[table]
		return t != nil && t.ColEncode[col] == EncodingYMD
	}}
	whereTime := false          // 固定条件里有 今天 / 现在: 例子要写 现在,式样书记下领域的时区
	for _, w := range a.Where { // 固定条件: 写法 / 列名 / 常量的类型在这里就检查
		if _, err := where.ParseOpt(w, compiler.WhereTypes(c.cat[table]), timeOpt); err != nil {
			e.add(1, "%v", err)
		}
		whereTime = whereTime || where.UsesTime(w)
	}
	for _, n := range nests { // 嵌套 / 子聚合的固定条件(子表的列;写法由编译器检查)
		for _, w := range n.Where {
			whereTime = whereTime || where.UsesTime(w)
		}
	}
	for _, g := range childAggs {
		for _, w := range g.Where {
			whereTime = whereTime || where.UsesTime(w)
		}
	}
	if whereTime {
		usesNow = true
		s.Datasource.TimeZone = c.d.TZ
		if a.Exit.AI && a.Kind == "custom" {
			e.add(1, "固定条件里的 今天 / 现在 不能用于 %s(查询由 AI 写)", a.Exit.Name)
		}
	}
	if err := e.err(); err != nil {
		return nil, "", err
	}
	if len(rename) > 0 {
		s.Datasource.Rename = rename
	}
	if len(a.UseColumns) > 0 { // 计算: 使用的列
		s.Datasource.Columns = a.UseColumns
	}
	for _, r := range a.Rules {
		s.Behavior = append(s.Behavior, spec.Behavior{When: r.When, Then: r.Then})
	}
	for _, qa := range a.Reviewed {
		s.ReviewDecisions = append(s.ReviewDecisions, spec.ReviewDecision{Question: qa.Question, Decision: qa.Answer})
	}
	if len(lookups)+len(nests)+len(childAggs) > 0 { // 例子: 准备数据按表的真实的列
		c.cat[table] = rawCols
	}
	if err := c.examples(a, s, req, resp, itemKey, usesNow); err != nil {
		return nil, "", err
	}
	var related []string // 准备数据里没写的关联 / 嵌套 / 子聚合的表 = 空表(不能留着别的用例的数据)
	switch {
	case a.Kind == "command": // 组合命令写的表: 准备数据里没写 = 空表
		related = append(related, compositeTables(a.Pattern.Command)...)
	case a.Exit.Command: // 命令写的表: 准备数据里没写 = 空表
		related = append(related, table)
	}
	for _, l := range lookups {
		related = append(related, l.Table)
	}
	for _, n := range nests {
		related = append(related, n.Table)
	}
	for _, g := range childAggs {
		related = append(related, g.Table)
	}
	for _, t := range related {
		if s.Acceptance.Fixtures == nil {
			s.Acceptance.Fixtures = map[string][]map[string]any{}
		}
		if _, ok := s.Acceptance.Fixtures[t]; !ok {
			s.Acceptance.Fixtures[t] = []map[string]any{}
		}
	}
	return s, fmt.Sprintf("  // %s\n  rpc %s(%s) returns (%s);", oneLine(a.Desc), a.Name, req.Name, resp.Name), nil
}

func (c *ctx) addMessage(m *message) {
	c.msgs = append(c.msgs, m)
	c.msgByNm[m.Name] = m
}

func sameFields(a, b *message) bool {
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for i := range a.Fields {
		if a.Fields[i].Name != b.Fields[i].Name || a.Fields[i].Type != b.Fields[i].Type {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 领域 → 机器用的 _domain.yaml
// ---------------------------------------------------------------------------

func (c *ctx) enumValueName(en, v string) string { return upperSnake(en) + "_" + v }

func (c *ctx) machineDomain() *spec.Domain {
	md := &spec.Domain{DomainVersion: spec.DomainVersion, Entities: map[string]spec.Entity{}}
	for name, t := range c.d.Tables {
		ent := spec.Entity{NaturalKey: t.PrimaryKey, Policy: t.Policy, Storage: t.Storage, ShardBy: t.ShardBy}
		for col, en := range t.ColEnum {
			e := c.d.Enum(en)
			vals := map[int64]string{}
			for _, v := range e.Values {
				vals[v.DB] = c.enumValueName(en, v.Name)
			}
			if ent.Columns == nil {
				ent.Columns = map[string]spec.ColumnMeta{}
			}
			ent.Columns[col] = spec.ColumnMeta{Enum: &spec.EnumMeta{Proto: c.pkg + "." + en, Values: vals}, OnUnknown: "unspecified"}
		}
		for col, enc := range t.ColEncode {
			if ent.Columns == nil {
				ent.Columns = map[string]spec.ColumnMeta{}
			}
			m := ent.Columns[col]
			m.Encoding = enc
			ent.Columns[col] = m
		}
		for _, r := range t.Rules {
			ent.Invariants = append(ent.Invariants, spec.Invariant{Rule: r.Text, Enforced: r.Enforced})
		}
		md.Entities[name] = ent
	}
	for _, qa := range c.d.Reviewed {
		md.ReviewDecisions = append(md.ReviewDecisions, spec.ReviewDecision{Question: qa.Question, Decision: qa.Answer})
	}
	return md
}

// ---------------------------------------------------------------------------
// proto
// ---------------------------------------------------------------------------

func (c *ctx) renderProto(rpcs []string, options []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// Code generated by factory from design/%s. DO NOT EDIT.\n// 人写的式样书在 design/%s/(这个文件由设计编译器生成)\n\nsyntax = \"proto3\";\n\npackage %s;\n\n", c.d.Name, c.d.Name, c.pkg)
	usesTS, usesValidate := false, false
	for _, m := range c.msgs {
		usesValidate = usesValidate || len(m.CEL) > 0
		for _, f := range m.Fields {
			usesValidate = usesValidate || len(f.Rules) > 0
			if f.Type == "google.protobuf.Timestamp" {
				usesTS = true
			}
		}
	}
	if usesValidate { // 没有任何校验规则时不 import(buf lint: unused import)
		b.WriteString("import \"buf/validate/validate.proto\";\n")
	}
	if usesTS {
		b.WriteString("import \"google/protobuf/timestamp.proto\";\n")
	}
	b.WriteString("\n")
	for _, o := range options {
		fmt.Fprintf(&b, "option %s;\n", o)
	}
	for _, en := range c.d.Enums {
		fmt.Fprintf(&b, "\n// %s\nenum %s {\n", oneLine(en.Desc), en.Name)
		prefix := upperSnake(en.Name) + "_"
		fmt.Fprintf(&b, "  %sUNSPECIFIED = 0;\n", prefix)
		for _, v := range en.Values {
			n := c.lock.number(en.Name, prefix+v.Name)
			fmt.Fprintf(&b, "  // %s\n  %s%s = %d;\n", oneLine(v.Desc), prefix, v.Name, n)
		}
		for _, r := range c.lock.reserved(en.Name) {
			fmt.Fprintf(&b, "  reserved %d;\n", r)
		}
		b.WriteString("}\n")
	}
	fmt.Fprintf(&b, "\n// %s\nservice %s {\n%s\n}\n", oneLine(c.d.Desc), c.service, strings.Join(rpcs, "\n\n"))
	for _, m := range c.msgs {
		fmt.Fprintf(&b, "\n// %s\nmessage %s {\n", oneLine(m.Desc), m.Name)
		for _, x := range m.CEL {
			fmt.Fprintf(&b, "  option (buf.validate.message).cel = {\n    id: %q\n    message: %q\n    expression: %q\n  };\n", x.ID, x.Message, x.Expr)
		}
		for _, f := range m.Fields {
			n := c.lock.number(m.Name, f.Name)
			if f.Desc != "" {
				fmt.Fprintf(&b, "  // %s\n", oneLine(f.Desc))
			}
			label := ""
			switch {
			case f.Repeated:
				label = "repeated "
			case f.Optional:
				label = "optional "
			}
			opts := ""
			if len(f.Rules) > 0 {
				var rs []string
				for _, r := range f.Rules {
					rs = append(rs, "(buf.validate.field)."+r)
				}
				opts = " [" + strings.Join(rs, ", ") + "]"
			}
			fmt.Fprintf(&b, "  %s%s %s = %d%s;\n", label, f.Type, f.Name, n, opts)
		}
		for _, r := range c.lock.reserved(m.Name) {
			fmt.Fprintf(&b, "  reserved %d;\n", r)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 命名
// ---------------------------------------------------------------------------

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "ses"):
		return strings.TrimSuffix(s, "es")
	case strings.HasSuffix(s, "s"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

func pascal(snake string) string {
	var b strings.Builder
	for _, p := range strings.Split(snake, "_") {
		if p != "" {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}

func upperSnake(pascal string) string {
	var b strings.Builder
	for i, r := range pascal {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

func kebab(pascal string) string {
	return strings.ReplaceAll(strings.ToLower(upperSnake(pascal)), "_", "-")
}

// ---------------------------------------------------------------------------
// 字段编号的锁文件: 只追加,不重排(对外发布后编号不能变)
// ---------------------------------------------------------------------------

type Lock struct {
	path    string
	Numbers map[string]map[string]int `yaml:"字段编号"` // message/enum → 字段名 → 编号
	used    map[string]map[string]bool
}

func loadLock(path string) (*Lock, error) {
	l := &Lock{path: path, Numbers: map[string]map[string]int{}, used: map[string]map[string]bool{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Numbers == nil {
		l.Numbers = map[string]map[string]int{}
	}
	return l, nil
}

func (l *Lock) number(scope, name string) int {
	if l.Numbers[scope] == nil {
		l.Numbers[scope] = map[string]int{}
	}
	if l.used[scope] == nil {
		l.used[scope] = map[string]bool{}
	}
	l.used[scope][name] = true
	if n, ok := l.Numbers[scope][name]; ok {
		return n
	}
	n := 1
	for _, v := range l.Numbers[scope] {
		n = max(n, v+1)
	}
	l.Numbers[scope][name] = n
	return n
}

// reserved: 以前用过、现在已经删掉的字段的编号(不能再给别的字段用)
func (l *Lock) reserved(scope string) []int {
	var out []int
	for name, n := range l.Numbers[scope] {
		if !l.used[scope][name] {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func (l *Lock) save() error {
	b, err := yaml.Marshal(l)
	if err != nil {
		return err
	}
	head := "# 由设计编译器维护,不要手改。proto 的字段编号对外发布后不能变: 只追加,删掉的字段编号保留(reserved)\n"
	return os.WriteFile(l.path, append([]byte(head), b...), 0o644)
}

// Kebab: GetJobSummary → get-job-summary(design 的文件名 → API 的 id)
func Kebab(api string) string { return kebab(api) }

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// lookups: 设计稿的 关联 → 机器用的式样书。关联的表 / 列是否存在、键是否唯一、分库由编译器检查(factory/compiler/lookup.go);
// 这里只检查设计稿层面的: 要有数据来源、关联的列不能是枚举(枚举的映射按来源的表查,还不支持别的表的枚举)。
func (c *ctx) lookups(a *API, table string, e *errs) []spec.Lookup {
	var out []spec.Lookup
	for _, l := range a.Lookups {
		if table == "" || a.Exit.AI {
			e.add(l.Line, "关联 %s: 只能用于有数据来源的查询(%s 不行)", l.Name, a.Exit.Name)
			continue
		}
		sl := spec.Lookup{Name: l.Name, Table: l.Table, On: l.On, Missing: l.Missing}
		for _, col := range l.Columns {
			if t := c.d.Tables[l.Table]; t != nil && t.ColEnum[col[1]] != "" {
				e.add(l.Line, "关联 %s: 列 %s(%s.%s)是枚举,关联的列还不能是枚举", l.Name, col[0], l.Table, col[1])
			}
			sl.Columns = append(sl.Columns, spec.TakeCol{Name: col[0], From: col[1]})
		}
		out = append(out, sl)
	}
	return out
}

// children: 设计稿的 嵌套 / 子聚合 → 机器用的式样书。表 / 列 / 键 / 分库 / 顺序的全序由编译器检查(factory/compiler/nest.go);
// 这里只检查设计稿层面的: 要有数据来源(AI 的出口不行)、子行的列不能是枚举(枚举的映射按来源的表查)。
func (c *ctx) children(a *API, table string, e *errs) ([]spec.Nest, []spec.ChildAgg) {
	var nests []spec.Nest
	var aggs []spec.ChildAgg
	for _, n := range a.Nests {
		if table == "" || a.Exit.AI {
			e.add(n.Line, "嵌套 %s: 只能用于有数据来源的查询(%s 不行)", n.Name, a.Exit.Name)
			continue
		}
		sn := spec.Nest{Name: n.Name, Table: n.Table, On: n.On, Where: n.Where, OrderBy: n.OrderBy, Max: n.Max}
		for _, col := range n.Columns {
			if t := c.d.Tables[n.Table]; t != nil && t.ColEnum[col[1]] != "" {
				e.add(n.Line, "嵌套 %s: 列 %s(%s.%s)是枚举,子行的列还不能是枚举", n.Name, col[0], n.Table, col[1])
			}
			sn.Columns = append(sn.Columns, spec.TakeCol{Name: col[0], From: col[1]})
		}
		nests = append(nests, sn)
	}
	for _, g := range a.ChildAggs {
		if table == "" || a.Exit.AI {
			e.add(g.Line, "子聚合 %s: 只能用于有数据来源的查询(%s 不行)", g.Name, a.Exit.Name)
			continue
		}
		if t := c.d.Tables[g.Table]; t != nil && g.Func != "count" && t.ColEnum[g.Column] != "" {
			e.add(g.Line, "子聚合 %s: 列 %s.%s 是枚举,不能聚合", g.Name, g.Table, g.Column)
		}
		aggs = append(aggs, spec.ChildAgg{Name: g.Name, Table: g.Table, On: g.On, Where: g.Where, Func: g.Func, Column: g.Column})
	}
	return nests, aggs
}

func nestNamed(a *API, name string) *Nest {
	for _, n := range a.Nests {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// nestItem: 嵌套的子行的消息(字段 = 子行的列,类型来自子表的列)。同名的消息已经有了时字段必须相同。
func (c *ctx) nestItem(n *Nest) (*message, error) {
	name := n.ItemName
	if name == "" {
		name = pascal(singular(n.Table))
	}
	item := &message{Name: name}
	if t := c.d.Tables[n.Table]; t != nil {
		item.Desc = t.Desc
	}
	for _, col := range n.Columns {
		pt, kind, opt, err := c.columnField(n.Table, col[1])
		if err != nil {
			return nil, err
		}
		desc := ""
		if t := c.d.Tables[n.Table]; t != nil {
			desc = t.ColDesc[col[1]]
		}
		item.Fields = append(item.Fields, &field{Name: col[0], Desc: desc, Type: pt, kind: kind, Optional: opt})
	}
	if prev, ok := c.msgByNm[item.Name]; ok {
		if !sameFields(prev, item) {
			return nil, fmt.Errorf("子行的消息 %s 已经被用过,但字段不同(请用 嵌套 的 行名 改名)", item.Name)
		}
		return prev, nil
	}
	c.addMessage(item)
	return item, nil
}

// checkPickUniquePlain: 普通的表(没有 存储)上的 每组取一条: 按 + 取 的列要覆盖表的主键,否则同一组里可能有多行
// 取的值相同,取哪一行不确定。以前只在验收时由参考求值器报 ambiguous order(批量压测: ListLatestJobPerType 按
// job_type 取 created_at 最大,创建时刻相同的任务);设计稿编译时就报。多版本的来源由 compiler.checkPickUnique 检查。
func checkPickUniquePlain(c *ctx, a *API, table string, e *errs) {
	g := a.Pattern.PickPerGroup
	if g == nil || table == "" {
		return
	}
	t := c.d.Tables[table]
	if t == nil || t.Storage != nil || len(t.PrimaryKey) == 0 {
		return
	}
	have := map[string]bool{}
	for _, k := range g.By {
		have[k] = true
	}
	var take []string
	for _, o := range g.Order {
		col, _, _ := strings.Cut(o, " ")
		have[col] = true
		take = append(take, col)
	}
	var miss []string
	for _, k := range t.PrimaryKey {
		if !have[k] {
			miss = append(miss, k)
		}
	}
	if len(miss) > 0 {
		e.add(1, "每组取一条: 按 %v + 取 %v 不能唯一确定一行(同一组里可能有多行取的值相同,取哪一行不确定)。"+
			"取 里加上 %v(%s 的主键),例: 取: %s 最大、%s 最大", g.By, take, miss, table, take[0], miss[0])
	}
}
