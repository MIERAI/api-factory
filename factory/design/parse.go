package design

// 中文式样书的读取。YAML 里的书写顺序有意义(输入/输出/枚举值的顺序决定字段编号),所以用 yaml.Node 保留顺序。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
	Rules      []Rule
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
		checkKeys(e, p.Val, "表 "+p.Key, "说明", "主键", "更新方式", "列", "规则")
		t := &Table{Name: p.Key, Desc: str(p.Val, "说明"), PrimaryKey: strList(get(p.Val, "主键")), ColDesc: map[string]string{}, ColEnum: map[string]string{}}
		switch str(p.Val, "更新方式") {
		case "", "可以更新":
			t.Policy = "mutable"
		case "只追加":
			t.Policy = "versioned"
		default:
			e.add(p.Line, "表 %s: 更新方式 只能是 可以更新 / 只追加", p.Key)
		}
		if len(t.PrimaryKey) == 0 {
			e.add(p.Line, "表 %s: 缺少 主键", p.Key)
		}
		for _, c := range pairs(get(p.Val, "列")) {
			if c.Val.Kind == yaml.ScalarNode {
				t.ColDesc[c.Key] = c.Val.Value
				continue
			}
			checkKeys(e, c.Val, "表 "+p.Key+" 的列 "+c.Key, "说明", "枚举")
			t.ColDesc[c.Key] = str(c.Val, "说明")
			if en := str(c.Val, "枚举"); en != "" {
				t.ColEnum[c.Key] = en
			}
		}
		if rs := get(p.Val, "规则"); rs != nil {
			for _, r := range rs.Content {
				checkKeys(e, r, "表 "+p.Key+" 的规则", "规则", "数据库保证")
				t.Rules = append(t.Rules, Rule{Text: str(r, "规则"), Enforced: str(r, "数据库保证") == "是"})
			}
		}
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
			checkKeys(e, q, "问答", "问题", "选项", "回答", "补充规则", "补充例子", "补充计算例子", "来自")
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
	Path        string
	Name        string // ListJobs
	Desc        string
	Background  string
	Owner       string
	Risk        string // low / medium / high
	Kind        string // get_by_id / list_keyset / compute / custom
	Source      string // 表
	UseColumns  []string
	Inputs      []*Input
	InputRules  []string
	Outputs     []*Output
	NotFound    string
	OrderBy     []string
	Desc_       bool // 排序方向
	PageDefault int32
	PageMax     int32
	ItemName    string // 一览的行 message 名(默认: 表名的单数形)
	ListField   string // 一览的列表字段名(默认: 表名)
	Rules       []Behavior
	Fixtures    *yaml.Node
	Cases       []*yaml.Node
	LogicCases  []*yaml.Node
	Reviewed    []QA // X.审查.yaml
}

type Input struct {
	Name     string
	Type     string // 整数32 / 整数64 / 文本 / 时间 / 布尔 / 小数 / <枚举名>
	Required bool
	Range    string
	Length   string
	Desc     string
	Filter   string // "created_at 大于等于"
	Line     int
}

type Output struct {
	Name     string
	Type     string // 空 = 和数据来源的同名列一致
	Optional bool
	Desc     string
	From     string // 来自的列(默认同名)
	Line     int
}

type Behavior struct{ When, Then string }

var kinds = map[string]string{"按键取一条": "get_by_id", "一览": "list_keyset", "计算": "compute", "自定义": "custom"}
var risks = map[string]string{"低": "low", "中": "medium", "高": "high"}

func loadAPI(path string) (*API, error) {
	root, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	e := &errs{file: path}
	checkKeys(e, root, "API", "API", "说明", "背景", "负责人", "风险", "种类", "数据来源", "使用的列", "输入", "输入之间的规则",
		"输出", "找不到时", "排序", "分页", "输出行名", "输出列表名", "业务规则", "例子", "计算例子")
	checkKeys(e, get(root, "例子"), "例子", "准备数据", "用例")
	checkKeys(e, get(root, "分页"), "分页", "默认每页", "每页最多")
	a := &API{
		Path: path, Name: str(root, "API"), Desc: str(root, "说明"), Background: str(root, "背景"),
		Owner: str(root, "负责人"), Source: str(root, "数据来源"), UseColumns: strList(get(root, "使用的列")),
		InputRules: strList(get(root, "输入之间的规则")), NotFound: str(root, "找不到时"),
		ItemName: str(root, "输出行名"), ListField: str(root, "输出列表名"),
		Fixtures: get(get(root, "例子"), "准备数据"),
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
	if a.Kind, ok = kinds[str(root, "种类")]; !ok {
		e.add(root.Line, "种类 只能是 按键取一条 / 一览 / 计算 / 自定义(现在是 %q)", str(root, "种类"))
	}
	if a.Risk, ok = risks[str(root, "风险")]; !ok {
		e.add(root.Line, "风险 只能是 低 / 中 / 高")
	}
	for _, p := range pairs(get(root, "输入")) {
		checkKeys(e, p.Val, "输入 "+p.Key, "类型", "必填", "范围", "长度", "说明", "筛选")
		in := &Input{Name: p.Key, Type: str(p.Val, "类型"), Required: str(p.Val, "必填") == "是", Range: str(p.Val, "范围"),
			Length: str(p.Val, "长度"), Desc: str(p.Val, "说明"), Filter: str(p.Val, "筛选"), Line: p.Line}
		if in.Type == "" {
			e.add(p.Line, "输入 %s: 缺少 类型", p.Key)
		}
		if f := str(p.Val, "必填"); f != "是" && f != "否" {
			e.add(p.Line, "输入 %s: 必填 只能是 是 / 否", p.Key)
		}
		a.Inputs = append(a.Inputs, in)
	}
	for _, p := range pairs(get(root, "输出")) {
		o := &Output{Name: p.Key, Line: p.Line}
		if p.Val.Kind == yaml.ScalarNode {
			o.Desc = p.Val.Value
		} else {
			checkKeys(e, p.Val, "输出 "+p.Key, "类型", "说明", "来自", "可以不设置")
			o.Type, o.Desc, o.From = str(p.Val, "类型"), str(p.Val, "说明"), str(p.Val, "来自")
			o.Optional = str(p.Val, "可以不设置") == "是"
		}
		a.Outputs = append(a.Outputs, o)
	}
	if len(a.Outputs) == 0 {
		e.add(root.Line, "缺少 输出")
	}
	if a.Kind == "list_keyset" {
		a.OrderBy, a.Desc_, err = parseOrder(strList(get(root, "排序")))
		if err != nil {
			e.add(root.Line, "%v", err)
		}
		pg := get(root, "分页")
		if _, err := fmt.Sscan(str(pg, "默认每页"), &a.PageDefault); err != nil {
			e.add(root.Line, "一览: 分页.默认每页 必须是整数")
		}
		if _, err := fmt.Sscan(str(pg, "每页最多"), &a.PageMax); err != nil {
			e.add(root.Line, "一览: 分页.每页最多 必须是整数")
		}
	}
	if a.Kind == "get_by_id" && a.NotFound == "" {
		e.add(root.Line, "按键取一条: 缺少 找不到时(找不到时返回的消息)")
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
			checkKeys(e, x, "用例", "说明", "输入", "结果")
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

func parseOrder(items []string) ([]string, bool, error) {
	if len(items) == 0 {
		return nil, false, fmt.Errorf("一览: 缺少 排序(例如 [created_at 新到旧, id 大到小])")
	}
	var cols []string
	var desc *bool
	for _, it := range items {
		f := strings.Fields(it)
		if len(f) != 2 {
			return nil, false, fmt.Errorf("排序 %q: 写成 \"列 新到旧\" 的形式", it)
		}
		d, ok := orderWords[f[1]]
		if !ok {
			return nil, false, fmt.Errorf("排序 %q: 方向只能是 新到旧/旧到新/大到小/小到大/降序/升序", it)
		}
		if desc != nil && *desc != d {
			return nil, false, fmt.Errorf("排序: 所有列的方向必须一致(现在混在一起)")
		}
		desc = &d
		cols = append(cols, f[0])
	}
	return cols, *desc, nil
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
