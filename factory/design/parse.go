package design

// 中文式样书的读取。YAML 里的书写顺序有意义(输入/输出/枚举值的顺序决定字段编号),所以用 yaml.Node 保留顺序。

import (
	"apifactory/factory/constraints"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/exits"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
)

// ---------------------------------------------------------------------------
// yaml.Node 工具
// ---------------------------------------------------------------------------

type pair struct {
	Key  string
	Val  *yaml.Node
	Line int
}

func readYAML(path string) (*yaml.Node, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w%s", path, err, yamlHint(err.Error()))
	}
	if len(doc.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	return doc.Content[0], nil
}

func pairs(n *yaml.Node) []pair {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out []pair
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, pair{Key: n.Content[i].Value, Val: n.Content[i+1], Line: n.Content[i].Line})
	}
	return out
}

func get(n *yaml.Node, key string) *yaml.Node {
	for _, p := range pairs(n) {
		if p.Key == key {
			return p.Val
		}
	}
	return nil
}

func str(n *yaml.Node, key string) string {
	if v := get(n, key); v != nil && v.Kind == yaml.ScalarNode {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

func strList(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, strings.TrimSpace(c.Value))
	}
	return out
}

// checkKeys: 只允许已知的键。花括号 { } 里的值如果有半角逗号,YAML 会把后半截当成一个新的键,
// 而且不报错 —— 规则会悄悄丢掉半句(实测)。所以遇到不认识的键一律报错。
func checkKeys(e *errs, n *yaml.Node, where string, allowed ...string) {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, p := range pairs(n) {
		if !ok[p.Key] {
			hint := ""
			if n.Style&yaml.FlowStyle != 0 || p.Val.Tag == "!!null" {
				hint = "。如果这是上一个值的后半截: 值里的半角逗号「,」会把值切断,请给值加引号,或改用全角逗号「，」"
			}
			e.add(p.Line, "%s: 不认识的键 %q(可以用: %s)%s", where, p.Key, strings.Join(allowed, " / "), hint)
		}
	}
}

// errf: 带文件名和行号的中文错误。
type errs struct {
	file string
	list []string
}

func (e *errs) add(line int, format string, args ...any) {
	e.list = append(e.list, fmt.Sprintf("%s:%d: %s", e.file, line, fmt.Sprintf(format, args...)))
}

func (e *errs) err() error {
	if len(e.list) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(e.list, "\n"))
}

// ---------------------------------------------------------------------------
// 领域
// ---------------------------------------------------------------------------

type Domain struct {
	Dir      string
	Name     string // jobs
	Desc     string
	TZ       string
	Enums    []*Enum
	Tables   map[string]*Table
	Reviewed []QA // 领域.审查.yaml
}

type Enum struct {
	Name      string // JobStatus
	Desc      string
	Values    []EnumValue
	OnUnknown string
}

type EnumValue struct {
	Name string // PENDING
	Desc string
	DB   int64
}

type Table struct {
	Name       string
	Desc       string
	PrimaryKey []string
	Policy     string            // mutable / versioned
	ColDesc    map[string]string // 列 → 说明
	ColEnum    map[string]string // 列 → 枚举名
	ColEncode  map[string]string // 列 → 编码(整数列里的日期: YYYYMMDD)
	Rules      []Rule
	Storage    *spec.Storage // 存储: JSON版本(逻辑表 → 物理表的映射)
	ShardBy    string        // 分库: 分库键的列(按店铺分库等)
}

type Rule struct {
	Text     string
	Enforced bool
}

type QA struct {
	Question string
	Options  []string
	Answer   string
	AddRules []Behavior
	AddCases []*yaml.Node // 补充例子(和 例子.用例 同格式)
	AddLogic []*yaml.Node // 补充计算例子(和 计算例子 同格式)
	From     string
}

func (d *Domain) Enum(name string) *Enum {
	for _, e := range d.Enums {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func loadDomain(dir string) (*Domain, error) {
	path := filepath.Join(dir, "领域.yaml")
	root, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	e := &errs{file: path}
	d := &Domain{Dir: dir, Name: str(root, "领域"), Desc: str(root, "说明"), TZ: str(root, "时区"), Tables: map[string]*Table{}}
	if d.Name == "" {
		e.add(root.Line, "缺少 领域(领域的英文名,例如 jobs)")
	}
	if d.TZ == "" {
		d.TZ = "UTC"
	}
	checkKeys(e, root, "领域", "领域", "说明", "时区", "枚举", "表")
	for _, p := range pairs(get(root, "枚举")) {
		checkKeys(e, p.Val, "枚举 "+p.Key, "说明", "值", "读到不认识的值")
		en := &Enum{Name: p.Key, Desc: str(p.Val, "说明"), OnUnknown: str(p.Val, "读到不认识的值")}
		for _, v := range pairs(get(p.Val, "值")) {
			checkKeys(e, v.Val, "枚举 "+p.Key+"."+v.Key, "说明", "数据库值")
			var db int64
			if _, err := fmt.Sscan(str(v.Val, "数据库值"), &db); err != nil {
				e.add(v.Line, "枚举 %s.%s: 数据库值 必须是整数", p.Key, v.Key)
			}
			en.Values = append(en.Values, EnumValue{Name: v.Key, Desc: str(v.Val, "说明"), DB: db})
		}
		if len(en.Values) == 0 {
			e.add(p.Line, "枚举 %s: 至少要有一个值", p.Key)
		}
		d.Enums = append(d.Enums, en)
	}
	for _, p := range pairs(get(root, "表")) {
		checkKeys(e, p.Val, "表 "+p.Key, append([]string{"说明", "主键", "更新方式", "列", "规则", "存储", "分库"}, sources.AllKeys()...)...)
		t := &Table{Name: p.Key, Desc: str(p.Val, "说明"), PrimaryKey: strList(get(p.Val, "主键")), ColDesc: map[string]string{}, ColEnum: map[string]string{}, ColEncode: map[string]string{}}
		switch str(p.Val, "更新方式") {
		case "", "可以更新":
			t.Policy = "mutable"
		case "只追加":
			t.Policy = "versioned"
		default:
			e.add(p.Line, "表 %s: 更新方式 只能是 可以更新 / 只追加", p.Key)
		}
		// 来源的注册表(factory/sources): 不写 存储 = 普通的表
		if src, ok := sources.ByName(str(p.Val, "存储")); !ok {
			e.add(p.Line, "表 %s: 存储 只能是 %s(不写 = 普通的表)", p.Key, sources.Names())
		} else if src.Parse == nil {
			for _, k := range sources.AllKeys() {
				if get(p.Val, k) != nil {
					e.add(p.Line, "表 %s: %s 只能和 存储 一起写", p.Key, k)
				}
			}
		} else {
			t.Policy = "versioned"
			t.Storage = src.Parse(tableReader{e: e, name: p.Key, n: p.Val, t: t})
		}
		if len(t.PrimaryKey) == 0 {
			e.add(p.Line, "表 %s: 缺少 主键", p.Key)
		}
		for _, c := range pairs(get(p.Val, "列")) {
			if c.Val.Kind == yaml.ScalarNode {
				t.ColDesc[c.Key] = c.Val.Value
				continue
			}
			checkKeys(e, c.Val, "表 "+p.Key+" 的列 "+c.Key, "说明", "枚举", "编码")
			t.ColDesc[c.Key] = str(c.Val, "说明")
			if en := str(c.Val, "枚举"); en != "" {
				t.ColEnum[c.Key] = en
			}
			if enc := str(c.Val, "编码"); enc != "" {
				if enc != EncodingYMD {
					e.add(c.Line, "表 %s 的列 %s: 编码 只能是 %s(整数里的日期)", p.Key, c.Key, EncodingYMD)
				}
				t.ColEncode[c.Key] = enc
			}
		}
		if rs := get(p.Val, "规则"); rs != nil {
			for _, r := range rs.Content {
				checkKeys(e, r, "表 "+p.Key+" 的规则", "规则", "数据库保证")
				t.Rules = append(t.Rules, Rule{Text: str(r, "规则"), Enforced: str(r, "数据库保证") == "是"})
			}
		}
		t.ShardBy = str(p.Val, "分库")
		d.Tables[p.Key] = t
	}
	for _, t := range d.Tables {
		for col, en := range t.ColEnum {
			if d.Enum(en) == nil {
				e.add(root.Line, "表 %s 的列 %s: 枚举 %s 没有定义", t.Name, col, en)
			}
		}
	}
	qas, err := loadQA(filepath.Join(dir, "领域.审查.yaml"))
	if err != nil {
		return nil, err
	}
	d.Reviewed = qas
	if err := e.err(); err != nil {
		return nil, err
	}
	return d, nil
}

func loadQA(path string) ([]QA, error) {
	e := &errs{file: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	root, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	var out []QA
	if list := get(root, "问答"); list != nil {
		for _, q := range list.Content {
			checkKeys(e, q, "问答", "问题", "选项", "同一个问题", "回答", "补充规则", "补充例子", "补充计算例子", "改了设计", "来自")
			qa := QA{Question: str(q, "问题"), Options: strList(get(q, "选项")), Answer: str(q, "回答"), From: str(q, "来自")}
			if r := get(q, "补充规则"); r != nil {
				for _, b := range seqOrOne(r) {
					checkKeys(e, b, "补充规则", "情况", "结果")
					qa.AddRules = append(qa.AddRules, Behavior{When: str(b, "情况"), Then: str(b, "结果")})
				}
			}
			if c := get(q, "补充例子"); c != nil {
				qa.AddCases = append(qa.AddCases, seqOrOne(c)...)
			}
			if c := get(q, "补充计算例子"); c != nil {
				qa.AddLogic = append(qa.AddLogic, seqOrOne(c)...)
			}
			out = append(out, qa)
		}
	}
	return out, e.err()
}

func seqOrOne(n *yaml.Node) []*yaml.Node {
	if n.Kind == yaml.SequenceNode {
		return n.Content
	}
	return []*yaml.Node{n}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

type API struct {
	Where      []string // 固定条件(factory/where)
	Path       string
	Name       string // ListJobs
	Desc       string
	Background string
	Owner      string
	Risk       string // low / medium / high
	Kind       string // get_by_id / list_keyset / compute / custom
	Source     string // 表
	UseColumns []string
	Inputs     []*Input
	InputRules []string
	Outputs    []*Output
	Exit       *exits.Exit  // 种类(出口)
	Pattern    spec.Pattern // 出口自己解析的设计稿(排序 / 分页 / 上限 / 找不到时 …)
	ItemName   string       // 一览的行 message 名(默认: 表名的单数形)
	ListField  string       // 一览的列表字段名(默认: 表名)
	Rules      []Behavior
	Fixtures   *yaml.Node
	Now        string     // 例子.现在: 用例的请求开始时刻(用到 现在 的 API 必须固定;用例可以各自写 现在)
	Derived    []*Derived // 派生参数(不出现在请求里)
	Cases      []*yaml.Node
	LogicCases []*yaml.Node
	Reviewed   []QA        // X.审查.yaml
	Lookups    []*Lookup   // 关联(多对一查找),按书写顺序
	Nests      []*Nest     // 嵌套(一对多: 子行的列表)
	ChildAggs  []*ChildAgg // 子聚合(按父行聚合)
}

// Nest: 嵌套的一项。`名字: { 表, 条件: [列 = 子表的列, …], 固定条件?, 排序: [列 小到大, …], 上限: N, 列: {…} | […], 行名? }`
type Nest struct {
	Name     string
	Table    string
	On       []string
	Where    []string
	OrderBy  []string // spec 的写法: "列 asc|desc"
	Max      int
	Columns  [][2]string // 子行的列名, 子表的列
	ItemName string      // 子行的消息名(没写 = 子表名的单数 PascalCase: order_items → OrderItem)
	Line     int
}

// ChildAgg: 子聚合的一项。`名字: { 表, 条件, 固定条件?, 计算: 件数 | 合计(列) | 最小(列) | 最大(列) }`
type ChildAgg struct {
	Name   string
	Table  string
	On     []string
	Where  []string
	Func   string // count / sum / min / max
	Column string
	Line   int
}

// Lookup: 关联的一项。`名字: { 表: …, 条件: [列 = 对方的列, …], 列: { 新列名: 对方的列 }, 找不到时: 保留 | 丢弃 }`
type Lookup struct {
	Name    string
	Table   string
	On      []string
	Columns [][2]string // 新列名, 对方的列
	Missing string      // keep / drop
	Line    int
}

type Input struct {
	Name        string
	Type        string // 整数32 / 整数64 / 文本 / 时间 / 布尔 / 小数 / <枚举名>
	Required    bool
	Constraints map[string]string // 约束(factory/constraints): 范围 / 长度 / 个数 …
	Desc        string
	Filter      string // "created_at 大于等于"
	Encoding    string // 编码: YYYYMMDD(整数的输入当作日期: 派生参数里可以算月初 / 加减;不合法的日期 → 参数错误)
	Write       string // 命令: 写入的列
	Key         string // 命令: 主键的列(按键更新 / 按键删除)
	Lock        string // 命令: 乐观锁的版本列
	Line        int
	Group       []*Input // 类型 组列表: 一个组的字段(名字 = 列名;组里的字段都要指定)
}

type Output struct {
	Name     string
	Type     string // 空 = 和数据来源的同名列一致
	Optional bool
	Desc     string
	From     string // 来自的列(默认同名)
	Line     int
	Rows     []*Output // 类型 列表: 每一行的字段(计算的输出)
}

type Behavior struct{ When, Then string }

var risks = map[string]string{"低": "low", "中": "medium", "高": "high"}

func loadAPI(path string) (*API, error) {
	root, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	e := &errs{file: path}
	baseKeys := TopKeys()
	checkKeys(e, get(root, "例子"), "例子", "准备数据", "用例", "现在")
	checkKeys(e, get(root, "分页"), "分页", "默认每页", "每页最多")
	a := &API{
		Path: path, Name: str(root, "API"), Desc: str(root, "说明"), Background: str(root, "背景"),
		Owner: str(root, "负责人"), Source: str(root, "数据来源"), UseColumns: strList(get(root, "使用的列")),
		InputRules: strList(get(root, "输入之间的规则")), Where: strList(get(root, "固定条件")),
		ItemName: str(root, "输出行名"), ListField: str(root, "输出列表名"),
		Fixtures: get(get(root, "例子"), "准备数据"), Now: str(get(root, "例子"), "现在"),
	}
	if a.Name == "" {
		e.add(root.Line, "缺少 API(API 的英文名,例如 ListJobs)")
	}
	if a.Desc == "" {
		e.add(root.Line, "缺少 说明")
	}
	if a.Owner == "" {
		e.add(root.Line, "缺少 负责人")
	}
	var ok bool
	if a.Exit, ok = exits.ByName(str(root, "种类")); ok { // 出口的注册表(factory/exits)
		a.Kind = a.Exit.Kind
		checkKeys(e, root, "API", append(baseKeys, a.Exit.DesignKeys...)...)
	} else {
		e.add(root.Line, "种类 只能是 %s(现在是 %q)", exits.Names(), str(root, "种类"))
		checkKeys(e, root, "API", baseKeys...)
	}
	if a.Risk, ok = risks[str(root, "风险")]; !ok {
		e.add(root.Line, "风险 只能是 低 / 中 / 高")
	}
	for _, p := range pairs(get(root, "关联")) {
		checkKeys(e, p.Val, "关联 "+p.Key, LookupKeys()...)
		l := &Lookup{Name: p.Key, Table: str(p.Val, "表"), On: strList(get(p.Val, "条件")), Line: p.Line}
		if l.Table == "" || len(l.On) == 0 {
			e.add(p.Line, "关联 %s: 要写 表 和 条件(例: 条件: [customer_id = id])", p.Key)
		}
		switch cols := get(p.Val, "列"); {
		case cols != nil && cols.Kind == yaml.MappingNode:
			for _, c := range pairs(cols) {
				l.Columns = append(l.Columns, [2]string{c.Key, strings.TrimSpace(c.Val.Value)})
			}
		case cols != nil && cols.Kind == yaml.SequenceNode: // [name, …] = 同名
			for _, c := range strList(cols) {
				l.Columns = append(l.Columns, [2]string{c, c})
			}
		}
		if len(l.Columns) == 0 {
			e.add(p.Line, "关联 %s: 要写 列(例: 列: { customer_name: name },新列名: 对方的列)", p.Key)
		}
		switch str(p.Val, "找不到时") {
		case "", "保留":
			l.Missing = "keep"
		case "丢弃":
			l.Missing = "drop"
		default:
			e.add(p.Line, "关联 %s: 找不到时 只能是 保留(加的列为空)/ 丢弃(这一行不要)", p.Key)
		}
		a.Lookups = append(a.Lookups, l)
	}
	for _, p := range pairs(get(root, "嵌套")) {
		checkKeys(e, p.Val, "嵌套 "+p.Key, NestKeys()...)
		n := &Nest{Name: p.Key, Table: str(p.Val, "表"), On: strList(get(p.Val, "条件")), Where: strOrList(get(p.Val, "固定条件")),
			ItemName: str(p.Val, "行名"), Line: p.Line}
		if n.Table == "" || len(n.On) == 0 {
			e.add(p.Line, "嵌套 %s: 要写 表 和 条件(例: 条件: [id = order_id])", p.Key)
		}
		cols, desc, err := parseOrder(strList(get(p.Val, "排序")))
		if err != nil {
			e.add(p.Line, "嵌套 %s: %v", p.Key, err)
		}
		for i, c := range cols {
			n.OrderBy = append(n.OrderBy, c+" "+map[bool]string{false: "asc", true: "desc"}[desc[i]])
		}
		if _, err := fmt.Sscan(str(p.Val, "上限"), &n.Max); err != nil || n.Max <= 0 {
			e.add(p.Line, "嵌套 %s: 上限 必须是正整数(子行多于上限时整个请求返回错误,不会悄悄截断)", p.Key)
		}
		switch cs := get(p.Val, "列"); {
		case cs != nil && cs.Kind == yaml.MappingNode:
			for _, c := range pairs(cs) {
				n.Columns = append(n.Columns, [2]string{c.Key, strings.TrimSpace(c.Val.Value)})
			}
		case cs != nil && cs.Kind == yaml.SequenceNode:
			for _, c := range strList(cs) {
				n.Columns = append(n.Columns, [2]string{c, c})
			}
		}
		if len(n.Columns) == 0 {
			e.add(p.Line, "嵌套 %s: 要写 列(子行的列,例: 列: [sku, qty, price])", p.Key)
		}
		a.Nests = append(a.Nests, n)
	}
	for _, p := range pairs(get(root, "子聚合")) {
		checkKeys(e, p.Val, "子聚合 "+p.Key, ChildAggKeys()...)
		g := &ChildAgg{Name: p.Key, Table: str(p.Val, "表"), On: strList(get(p.Val, "条件")), Where: strOrList(get(p.Val, "固定条件")), Line: p.Line}
		if g.Table == "" || len(g.On) == 0 {
			e.add(p.Line, "子聚合 %s: 要写 表 和 条件(例: 条件: [id = order_id])", p.Key)
		}
		calc := strings.TrimSpace(str(p.Val, "计算"))
		if m := reAggCall.FindStringSubmatch(calc); calc == "件数" {
			g.Func = "count"
		} else if m != nil {
			g.Func, g.Column = aggWords[m[1]], strings.TrimSpace(m[2])
		} else {
			e.add(p.Line, "子聚合 %s: 计算 写成 件数 / 合计(列) / 最小(列) / 最大(列),现在是 %q", p.Key, calc)
		}
		a.ChildAggs = append(a.ChildAggs, g)
	}
	for _, p := range pairs(get(root, "输入")) {
		checkKeys(e, p.Val, "输入 "+p.Key, append(InputKeys(), constraints.Keys()...)...)
		in := &Input{Name: p.Key, Type: str(p.Val, "类型"), Required: str(p.Val, "必填") == "是",
			Desc: str(p.Val, "说明"), Filter: str(p.Val, "筛选"), Encoding: str(p.Val, "编码"), Line: p.Line, Constraints: map[string]string{},
			Write: str(p.Val, "写入"), Key: str(p.Val, "键"), Lock: str(p.Val, "乐观锁")}
		for _, k := range constraints.Keys() {
			if v := str(p.Val, k); v != "" {
				in.Constraints[k] = v
			}
		}
		if in.Type == "" {
			e.add(p.Line, "输入 %s: 缺少 类型", p.Key)
		}
		if f := str(p.Val, "必填"); f != "是" && f != "否" {
			e.add(p.Line, "输入 %s: 必填 只能是 是 / 否", p.Key)
		}
		if g := get(p.Val, "组"); g != nil {
			if in.Type != GroupListType {
				e.add(p.Line, "输入 %s: 只有 类型 %s 才能写 组", p.Key, GroupListType)
			}
			for _, m := range pairs(g) {
				checkKeys(e, m.Val, "输入 "+p.Key+" 的组 "+m.Key, append([]string{"类型", "说明"}, constraints.Keys()...)...)
				mi := &Input{Name: m.Key, Type: str(m.Val, "类型"), Required: true, Desc: str(m.Val, "说明"), Line: m.Line, Constraints: map[string]string{}}
				for _, k := range constraints.Keys() {
					if v := str(m.Val, k); v != "" {
						mi.Constraints[k] = v
					}
				}
				in.Group = append(in.Group, mi)
			}
		} else if in.Type == GroupListType {
			e.add(p.Line, "输入 %s: 类型 %s 要写 组(组里的字段,名字 = 列名)", p.Key, GroupListType)
		}
		a.Inputs = append(a.Inputs, in)
	}
	for _, p := range pairs(get(root, "派生参数")) {
		checkKeys(e, p.Val, "派生参数 "+p.Key, DerivedKeys()...)
		d := &Derived{Name: p.Key, Expr: str(p.Val, "计算"), Filter: str(p.Val, "筛选"), Desc: str(p.Val, "说明"), Line: p.Line}
		if d.Expr == "" || d.Filter == "" {
			e.add(p.Line, "派生参数 %s: 要写 计算 和 筛选(例: { 计算: 现在 - 5 分钟, 筛选: updated_at 小于 })", p.Key)
		}
		a.Derived = append(a.Derived, d)
	}
	for _, p := range pairs(get(root, "输出")) {
		o := &Output{Name: p.Key, Line: p.Line}
		if p.Val.Kind == yaml.ScalarNode {
			o.Desc = p.Val.Value
		} else {
			checkKeys(e, p.Val, "输出 "+p.Key, OutputKeys()...)
			o.Type, o.Desc, o.From = str(p.Val, "类型"), str(p.Val, "说明"), str(p.Val, "来自")
			o.Optional = str(p.Val, "可以不设置") == "是"
			if o.Type == "列表" { // 计算的输出: 行的列表(每行 = 每一行的字段)
				for _, q := range pairs(get(p.Val, "每行")) {
					checkKeys(e, q.Val, "输出 "+p.Key+" 的每行 "+q.Key, "类型", "说明", "可以不设置")
					o.Rows = append(o.Rows, &Output{Name: q.Key, Line: q.Line, Type: str(q.Val, "类型"), Desc: str(q.Val, "说明"),
						Optional: str(q.Val, "可以不设置") == "是"})
				}
				if len(o.Rows) == 0 {
					e.add(p.Line, "输出 %s: 类型 列表 需要 每行(每一行的字段和类型)", p.Key)
				}
			}
		}
		a.Outputs = append(a.Outputs, o)
	}
	if len(a.Outputs) == 0 {
		e.add(root.Line, "缺少 输出")
	}
	if a.Exit != nil && a.Exit.Parse != nil { // 种类自己的键
		a.Exit.Parse(designReader{e, root}, &a.Pattern)
	}
	if rs := get(root, "业务规则"); rs != nil {
		for _, r := range rs.Content {
			checkKeys(e, r, "业务规则", "情况", "结果")
			a.Rules = append(a.Rules, Behavior{When: str(r, "情况"), Then: str(r, "结果")})
		}
	}
	if c := get(get(root, "例子"), "用例"); c != nil {
		a.Cases = c.Content
		for _, x := range c.Content {
			checkKeys(e, x, "用例", "说明", "输入", "结果", "现在", "错误", "之后")
		}
	}
	if c := get(root, "计算例子"); c != nil {
		a.LogicCases = c.Content
		for _, x := range c.Content {
			checkKeys(e, x, "计算例子", "说明", "输入", "输入行", "结果")
		}
	}
	review := strings.TrimSuffix(path, ".yaml") + ".审查.yaml"
	if a.Reviewed, err = loadQA(review); err != nil {
		e.list = append(e.list, err.Error())
	}
	for _, qa := range a.Reviewed {
		a.Rules = append(a.Rules, qa.AddRules...)
		a.Cases = append(a.Cases, qa.AddCases...)
		a.LogicCases = append(a.LogicCases, qa.AddLogic...)
	}
	if err := e.err(); err != nil {
		return nil, err
	}
	return a, nil
}

var orderWords = map[string]bool{"新到旧": true, "大到小": true, "降序": true, "旧到新": false, "小到大": false, "升序": false}

func parseOrder(items []string) ([]string, []bool, error) {
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("一览: 缺少 排序(例如 [created_at 新到旧, id 大到小])")
	}
	var cols []string
	var desc []bool
	for _, it := range items {
		f := strings.Fields(it)
		if len(f) != 2 {
			return nil, nil, fmt.Errorf("排序 %q: 写成 \"列 新到旧\" 的形式", it)
		}
		d, ok := orderWords[f[1]]
		if !ok {
			return nil, nil, fmt.Errorf("排序 %q: 方向只能是 新到旧/旧到新/大到小/小到大/降序/升序", it)
		}
		desc = append(desc, d)
		cols = append(cols, f[0])
	}
	return cols, desc, nil
}

// LoadDir: design/<领域>/ 下的 领域.yaml 和所有 API。
func LoadDir(dir string) (*Domain, []*API, error) {
	d, err := loadDomain(dir)
	if err != nil {
		return nil, nil, err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	sort.Strings(files)
	var apis []*API
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "领域") || strings.HasSuffix(base, ".审查.yaml") || strings.HasSuffix(base, ".lock.yaml") {
			continue
		}
		a, err := loadAPI(f)
		if err != nil {
			return nil, nil, err
		}
		apis = append(apis, a)
	}
	return d, apis, nil
}

// yamlHint: 常见的 YAML 书写错误 → 中文的原因和改法
func yamlHint(msg string) string {
	switch {
	case strings.Contains(msg, "mapping values are not allowed"):
		return "\n  → 这一行的值里有「冒号 + 空格」(例如 \"例: ...\"),YAML 会把它当成新的键。请把整个值用引号括起来: 问题: \"...例: ...\""
	case strings.Contains(msg, "did not find expected key"):
		return "\n  → 可能是值以引号开头、后面又跟了别的文字(例如 \"未发货\"同时...)。请把整个值用引号括起来,里面的引号换成「」"
	case strings.Contains(msg, "found character that cannot start any token"):
		return "\n  → 可能用了 Tab 缩进。YAML 只能用空格缩进"
	}
	return ""
}

// designReader: 出口读设计稿用(exits.Design)。
type designReader struct {
	e    *errs
	root *yaml.Node
}

func (r designReader) Str(key string) string      { return str(r.root, key) }
func (r designReader) Node(key string) *yaml.Node { return get(r.root, key) }
func (r designReader) Order(key string) ([]string, []bool, error) {
	return parseOrder(strList(get(r.root, key)))
}
func (r designReader) OrderList(items []string) ([]string, []bool, error) { return parseOrder(items) }
func (r designReader) Err(format string, args ...any)                     { r.e.add(r.root.Line, format, args...) }

// tableReader: 来源读领域里一张表的写法(sources.Domain)。
type tableReader struct {
	e    *errs
	name string
	n    *yaml.Node
	t    *Table
}

func (r tableReader) Name() string         { return r.name }
func (r tableReader) Node() *yaml.Node     { return r.n }
func (r tableReader) PrimaryKey() []string { return r.t.PrimaryKey }
func (r tableReader) CheckKeys(n *yaml.Node, where string, keys ...string) {
	checkKeys(r.e, n, where, keys...)
}
func (r tableReader) Err(line int, format string, args ...any) { r.e.add(line, format, args...) }
func (r tableReader) Column(name, desc, enum string) {
	r.t.ColDesc[name] = desc
	if enum != "" {
		r.t.ColEnum[name] = enum
	}
}

// TopKeys: 设计稿顶层的键(所有种类共用;各种类另外的键在出口的注册表里)。
func TopKeys() []string {
	return []string{"API", "说明", "背景", "负责人", "风险", "种类", "数据来源", "关联", "嵌套", "子聚合", "输入", "输入之间的规则", "固定条件", "派生参数",
		"输出", "输出行名", "输出列表名", "业务规则", "例子", "计算例子"}
}

// OutputKeys: 输出的一列可以写的键。
func OutputKeys() []string {
	return []string{"类型", "说明", "来自", "可以不设置", "每行"}
}

// NestKeys / ChildAggKeys: 嵌套 / 子聚合的一项可以写的键。
func NestKeys() []string {
	return []string{"表", "条件", "固定条件", "排序", "上限", "列", "行名"}
}
func ChildAggKeys() []string { return []string{"表", "条件", "固定条件", "计算"} }

// aggWords: 子聚合的 计算 的写法 → 聚合(core/agg.go)。件数 = count(没有列)。
var (
	aggWords  = map[string]string{"合计": "sum", "最小": "min", "最大": "max"}
	reAggCall = regexp.MustCompile(`^(合计|最小|最大)\s*[((]\s*([A-Za-z_][A-Za-z0-9_]*)\s*[))]$`)
)

// strOrList: 一个字符串或字符串的列表(固定条件)。
func strOrList(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		if v := strings.TrimSpace(n.Value); v != "" {
			return []string{v}
		}
		return nil
	}
	return strList(n)
}

// LookupKeys: 关联的一项可以写的键。
func LookupKeys() []string { return []string{"表", "条件", "列", "找不到时"} }

// InputKeys: 输入可以写的键(约束的键另外由 factory/constraints 决定)。
func InputKeys() []string {
	return []string{"类型", "必填", "说明", "筛选", "组", "编码", "写入", "键", "乐观锁"}
}

// EncodingYMD: 整数编码的日期(20260131 = 2026-01-31)。列(领域的 列: { 编码 })和输入(编码)都可以写。
const EncodingYMD = "YYYYMMDD"

// ymdCEL: 整数是合法的 YYYYMMDD(年 1000~9999、月 1~12、日 1~那个月的天数;闰年按公历)。核心的 ymd 认为合法的范围。
const ymdCEL = "this >= 10000101 && this <= 99991231 && (this / 100) % 100 >= 1 && (this / 100) % 100 <= 12 && this % 100 >= 1 && " +
	"this % 100 <= [31, ((this / 10000) % 4 == 0 && ((this / 10000) % 100 != 0 || (this / 10000) % 400 == 0)) ? 29 : 28, " +
	"31, 30, 31, 30, 31, 31, 30, 31, 30, 31][(this / 100) % 100 - 1]"

// GroupListType: 组的列表(每个组几个字段,和 筛选: 组属于 一起用: 有一个组逐列相等就留下)。
const GroupListType = "组列表"
