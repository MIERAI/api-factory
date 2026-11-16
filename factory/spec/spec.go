// Package spec 定义式样书格式并做机械校验(式样书闸门)。
//
// v0.2 的原则: 式样书里的每一行都应该是"只有人才能做的决定"。
// 能从 proto / schema / 模式 / 目录结构推导出来的东西不写:
//   - API id        ← 目录(specs/<domain>/<name>/spec.yaml → <domain>.<name>)
//   - proto 文件    ← rpc 名(buf 描述符)
//   - summary       ← proto 里 rpc 的注释
//   - 查询名        ← rpc 的方法名
//   - cardinality   ← pattern(get_by_id → one,其他 → many)
//   - SQL           ← datasource.table + filters + 响应字段(声明不了时才用 datasource.sql)
//   - AI 可编辑文件 ← pattern + 方法名
//   - 平台/模式已经保证的行为(NOT_FOUND / INVALID_ARGUMENT / UNAVAILABLE) ← 不写进 behavior
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const Version = "0.2"

type Spec struct {
	Path        string     `yaml:"-"`
	Domain      *Domain    `yaml:"-"` // specs/<domain>/_domain.yaml(可选)
	SpecVersion string     `yaml:"spec_version"`
	API         API        `yaml:"api"`
	Pattern     Pattern    `yaml:"pattern"`
	Datasource  Datasource `yaml:"datasource"`
	// Behavior: 只写业务规则。compute / custom 必填,模式 API 一般不需要
	Behavior   []Behavior `yaml:"behavior"`
	Acceptance Acceptance `yaml:"acceptance"`
	// ReviewDecisions: 式样书审查提出的问题和人的决定(包括"不处理")。以后的审查不再提出
	ReviewDecisions []ReviewDecision `yaml:"review_decisions"`
}

type API struct {
	RPC         string `yaml:"rpc"`
	Owner       string `yaml:"owner"`
	RiskLevel   string `yaml:"risk_level"`
	Description string `yaml:"description"` // 可选: proto 注释之外的背景说明
}

type ReviewDecision struct {
	Question string `yaml:"question"`
	Decision string `yaml:"decision"`
}

// Pattern 决定 handler 由谁写:
//   - get_by_id   : 工厂生成。按请求字段查 1 行,查不到 → NOT_FOUND
//   - list_keyset : 工厂生成。筛选 + keyset 分页
//   - compute     : 工厂生成 handler(映射 + 查询),AI 只写纯函数 <method>Logic(req, rows)
//   - custom      : AI 写整个 handler(逃生口)
type Pattern struct {
	Kind            string   `yaml:"kind"`
	NotFoundMessage string   `yaml:"not_found_message"` // get_by_id: "job not found: {job_id}"
	OrderBy         []string `yaml:"order_by"`          // list_keyset: 游标列,最后一列必须唯一
	Direction       string   `yaml:"direction"`         // list_keyset: desc / asc
	PageSize        PageSize `yaml:"page_size"`
}

type PageSize struct {
	Default int32 `yaml:"default"`
	Max     int32 `yaml:"max"`
}

// Datasource: 声明式(table + filters)或逃生口(sql),二选一。只支持读取。
type Datasource struct {
	Table string `yaml:"table"`
	// Filters: 请求字段 → 列 + 运算符。符合命名约定的可以省略:
	//   字段名 = 列名 → eq;  <x>_after → <x>_at gte;  <x>_before → <x>_at lt;  <单数表名>_id → id eq
	// 有 presence 的字段(optional / Timestamp)未指定时不筛选;其他字段总是参与筛选
	Filters map[string]Filter `yaml:"filters"`
	// Columns: SELECT 的列。默认从响应字段推导;compute 的输入是逻辑函数用的列,需要显式写
	Columns []string `yaml:"columns"`
	// SQL: 逃生口(JOIN / 聚合 / 复杂条件)。写了 sql 就不写 table/filters/columns
	SQL string `yaml:"sql"`
}

type Filter struct {
	Column string `yaml:"column"`
	Op     string `yaml:"op"` // eq / gt / gte / lt / lte
}

var FilterOps = map[string]string{"eq": "=", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

type Behavior struct {
	When string `yaml:"when"`
	Then string `yaml:"then"`
}

type Acceptance struct {
	Fixtures map[string][]map[string]any `yaml:"fixtures"`
	Cases    []Case                      `yaml:"cases"`
}

type Case struct {
	Name    string         `yaml:"name"`
	Request map[string]any `yaml:"request"`
	Expect  Expect         `yaml:"expect"`
	// Steps 用于多步用例(如翻页)。request 里的 "$prev.<field>" 会被替换成上一步响应的字段
	Steps []Step `yaml:"steps"`
}

type Step struct {
	Request map[string]any `yaml:"request"`
	Expect  Expect         `yaml:"expect"`
}

// AllSteps 把单步用例也统一成 steps 形式。
func (c Case) AllSteps() []Step {
	if len(c.Steps) > 0 {
		return c.Steps
	}
	return []Step{{Request: c.Request, Expect: c.Expect}}
}

type Expect struct {
	Code     string         `yaml:"code"`
	Response map[string]any `yaml:"response"`
}

// ---------------------------------------------------------------------------
// 推导出来的属性
// ---------------------------------------------------------------------------

// ID: specs/<domain>/<name>/spec.yaml → <domain>.<name>
func (s *Spec) ID() string {
	dir := filepath.Dir(s.Path)
	return filepath.Base(filepath.Dir(dir)) + "." + filepath.Base(dir)
}

func (s *Spec) PatternKind() string {
	if s.Pattern.Kind == "" {
		return "custom"
	}
	return s.Pattern.Kind
}

// AIWrites: 这个 API 是否有 AI 负责的部分。
func (s *Spec) AIWrites() bool {
	k := s.PatternKind()
	return k == "custom" || k == "compute"
}

// QueryName: 查询函数名 = rpc 方法名。
func (s *Spec) QueryName() string {
	p, _ := s.RPCParts()
	return p.Method
}

// Cardinality: 由模式决定。
func (s *Spec) Cardinality() string {
	if s.PatternKind() == "get_by_id" {
		return "one"
	}
	return "many"
}

// AIMayEdit: AI 可以创建/修改的文件。compute 的第一个是逻辑文件。
func (s *Spec) AIMayEdit() []string {
	p, err := s.RPCParts()
	if err != nil {
		return nil
	}
	dir := "internal/handler/" + p.Domain() + "/"
	base := Snake(p.Method)
	switch s.PatternKind() {
	case "compute":
		return []string{dir + base + "_logic.go", dir + base + "_logic_test.go"}
	case "custom":
		return []string{dir + base + ".go", dir + base + "_test.go"}
	}
	return nil
}

// RPCParts 是 rpc 字段拆解后的结果。
type RPCParts struct {
	ProtoPackage string // factory.jobs.v1
	Service      string // JobService
	Method       string // GetJobStatus
}

var rpcRe = regexp.MustCompile(`^([a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)*)\.([A-Z][A-Za-z0-9]*)/([A-Z][A-Za-z0-9]*)$`)

func (s *Spec) RPCParts() (RPCParts, error) {
	m := rpcRe.FindStringSubmatch(s.API.RPC)
	if m == nil {
		return RPCParts{}, fmt.Errorf("api.rpc must look like pkg.v1.Service/Method, got %q", s.API.RPC)
	}
	return RPCParts{ProtoPackage: m[1], Service: m[2], Method: m[3]}, nil
}

// Domain 返回 proto 包里去掉前缀和版本号后的领域名(factory.jobs.v1 → jobs)。
func (p RPCParts) Domain() string {
	segs := strings.Split(p.ProtoPackage, ".")
	if len(segs) >= 2 && regexp.MustCompile(`^v\d+`).MatchString(segs[len(segs)-1]) {
		return segs[len(segs)-2]
	}
	return segs[len(segs)-1]
}

// Snake: GetJobStatus → get_job_status
func Snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // 拼错或已废弃的字段直接报错
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Path = path
	if s.Domain, err = LoadDomain(DomainPath(path)); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadAll 读取 dir 下所有 spec.yaml。
func LoadAll(dir string) ([]*Spec, error) {
	var specs []*Spec
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "spec.yaml" {
			s, err := Load(p)
			if err != nil {
				return err
			}
			specs = append(specs, s)
		}
		return nil
	})
	return specs, err
}

// ---------------------------------------------------------------------------
// 式样书闸门(只做不需要 proto / schema 的机械检查;那部分在 prepare 里检查)
// ---------------------------------------------------------------------------

var writeSQL = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|drop|alter|create|grant|revoke)\b`)

func (s *Spec) Check() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if s.Domain != nil {
		for _, e := range s.Domain.Check() {
			add("%s: %s", s.Domain.Path, e)
		}
	}
	if s.SpecVersion != Version {
		add("spec_version: unsupported %q (want %q)", s.SpecVersion, Version)
	}
	if s.API.Owner == "" {
		add("api.owner is required")
	}
	if _, err := s.RPCParts(); err != nil {
		add("%v", err)
	}
	switch s.API.RiskLevel {
	case "low", "medium", "high":
	default:
		add("api.risk_level must be low/medium/high")
	}

	// datasource: 声明式 / 逃生口 二选一
	ds := s.Datasource
	declarative := ds.Table != "" || len(ds.Filters) > 0 || len(ds.Columns) > 0
	switch {
	case ds.SQL != "" && declarative:
		add("datasource: use either table/filters/columns or sql, not both")
	case ds.SQL == "" && ds.Table == "":
		add("datasource: table (declarative) or sql (escape hatch) is required")
	}
	if ds.SQL != "" && writeSQL.MatchString(ds.SQL) {
		add("datasource.sql: only read queries are supported")
	}
	for field, f := range ds.Filters {
		if f.Column == "" {
			add("datasource.filters.%s: column is required", field)
		}
		if _, ok := FilterOps[f.Op]; !ok {
			add("datasource.filters.%s: op must be eq/gt/gte/lt/lte (got %q)", field, f.Op)
		}
	}
	if s.PatternKind() == "compute" && ds.SQL == "" && len(ds.Columns) == 0 {
		add("datasource.columns: required for pattern compute (the logic function's input cannot be derived from the response)")
	}

	for i, d := range s.ReviewDecisions {
		if d.Question == "" || d.Decision == "" {
			add("review_decisions[%d]: question and decision are required", i)
		}
	}
	if s.AIWrites() && len(s.Behavior) == 0 {
		add("behavior: required for pattern %s (it is the spec of what AI implements)", s.PatternKind())
	}
	if len(s.Acceptance.Cases) == 0 {
		add("acceptance.cases: at least one case is required")
	}
	for i, c := range s.Acceptance.Cases {
		if c.Name == "" {
			add("acceptance.cases[%d]: name is required", i)
		}
		if len(c.Steps) > 0 && (c.Request != nil || c.Expect.Code != "") {
			add("acceptance.cases[%d]: use either request/expect or steps, not both", i)
		}
		for j, st := range c.AllSteps() {
			if st.Expect.Code == "" {
				add("acceptance.cases[%d].steps[%d]: expect.code is required", i, j)
			}
		}
	}
	if ds.Table != "" {
		for table := range s.Acceptance.Fixtures {
			if table != ds.Table {
				add("acceptance.fixtures: table %s is not datasource.table (%s)", table, ds.Table)
			}
		}
	}

	switch s.PatternKind() {
	case "custom", "compute":
	case "get_by_id":
		if s.Pattern.NotFoundMessage == "" {
			add("pattern get_by_id: not_found_message is required")
		}
	case "list_keyset":
		if len(s.Pattern.OrderBy) == 0 {
			add("pattern list_keyset: order_by is required (last column must be unique, e.g. id)")
		}
		if s.Pattern.Direction != "asc" && s.Pattern.Direction != "desc" {
			add("pattern list_keyset: direction must be asc or desc")
		}
		if ps := s.Pattern.PageSize; ps.Default <= 0 || ps.Max < ps.Default {
			add("pattern list_keyset: page_size.default > 0 and max >= default are required")
		}
		if regexp.MustCompile(`(?i)\b(order\s+by|limit|offset)\b`).MatchString(ds.SQL) {
			add("pattern list_keyset: sql must not contain ORDER BY / LIMIT / OFFSET (the factory adds them)")
		}
	default:
		add("pattern.kind must be custom / compute / get_by_id / list_keyset")
	}
	return errs
}

// ---------------------------------------------------------------------------
// 自动生成的验收用例(gen/acceptance/<id>.yaml)
//   人写的用例验证"意图";自动生成的用例验证"机制"(工厂和平台有没有把声明正确地变成代码)
// ---------------------------------------------------------------------------

type AutoAcceptance struct {
	Spec     string   `yaml:"spec"`
	Coverage []string `yaml:"coverage"` // 自动覆盖了哪些方面(给人和审查员看)
	Suites   []Suite  `yaml:"suites"`
}

// Suite: Fixtures 为 nil 时沿用当前 DB 内容(不重新灌数据)。
type Suite struct {
	Name     string                      `yaml:"name"`
	Fixtures map[string][]map[string]any `yaml:"fixtures,omitempty"`
	Cases    []Case                      `yaml:"cases"`
}

func AutoAcceptancePath(id string) string { return "gen/acceptance/" + id + ".yaml" }

func LoadAutoAcceptance(id string) (*AutoAcceptance, error) {
	b, err := os.ReadFile(AutoAcceptancePath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a AutoAcceptance
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", AutoAcceptancePath(id), err)
	}
	return &a, nil
}
