// Package spec 定义式样书格式并做机械校验(式样书闸门)。
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Path          string        `yaml:"-"`
	SpecVersion   string        `yaml:"spec_version"`
	API           API           `yaml:"api"`
	Pattern       Pattern       `yaml:"pattern"`
	Datasource    Datasource    `yaml:"datasource"`
	Behavior      []Behavior    `yaml:"behavior"`
	NonFunctional NonFunctional `yaml:"non_functional"`
	Acceptance    Acceptance    `yaml:"acceptance"`
	Generation    Generation    `yaml:"generation"`
}

type API struct {
	ID          string `yaml:"id"`
	Summary     string `yaml:"summary"`
	Description string `yaml:"description"`
	RPC         string `yaml:"rpc"`
	Proto       string `yaml:"proto"`
	Owner       string `yaml:"owner"`
	RiskLevel   string `yaml:"risk_level"`
}

// Pattern 决定 handler 由谁写:
//   - custom      : AI 写(generation.ai_may_edit 必填)
//   - get_by_id   : 工厂生成。按参数查 1 行,查不到 → NOT_FOUND
//   - list_keyset : 工厂生成。筛选 + keyset 分页(游标/ORDER BY/LIMIT 由工厂追加到查询)
type Pattern struct {
	Kind            string   `yaml:"kind"`
	NotFoundMessage string   `yaml:"not_found_message"` // get_by_id: "job not found: {job_id}"
	OrderBy         []string `yaml:"order_by"`          // list_keyset: 游标列,必须能唯一确定顺序
	Direction       string   `yaml:"direction"`         // list_keyset: desc / asc
	PageSize        PageSize `yaml:"page_size"`
	// Mapping: 查询参数名 → 请求字段名。只在名字对不上时写(默认按名字自动对应)
	Mapping map[string]string `yaml:"mapping"`
}

type PageSize struct {
	Default int32 `yaml:"default"`
	Max     int32 `yaml:"max"`
}

func (s *Spec) PatternKind() string {
	if s.Pattern.Kind == "" {
		return "custom"
	}
	return s.Pattern.Kind
}

type Datasource struct {
	Access string  `yaml:"access"`
	Tables []Table `yaml:"tables"`
	Query  Query   `yaml:"query"`
}

type Table struct {
	Name    string   `yaml:"name"`
	Columns []string `yaml:"columns"`
}

type Query struct {
	Name        string `yaml:"name"`
	Cardinality string `yaml:"cardinality"`
	SQL         string `yaml:"sql"`
}

type Behavior struct {
	When string `yaml:"when"`
	Then string `yaml:"then"`
}

type NonFunctional struct {
	LatencyP99Ms int `yaml:"latency_p99_ms"`
	TimeoutMs    int `yaml:"timeout_ms"`
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

type Generation struct {
	AIMayEdit []string `yaml:"ai_may_edit"`
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

func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // 拼错的字段直接报错
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Path = path
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

var writeSQL = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|drop|alter|create|grant|revoke)\b`)

// Check 是式样书闸门: 只做机械可判定的检查。
func (s *Spec) Check() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if s.SpecVersion != "0.1" {
		add("spec_version: unsupported %q (want \"0.1\")", s.SpecVersion)
	}
	if s.API.ID == "" || s.API.Summary == "" || s.API.Owner == "" {
		add("api.id / api.summary / api.owner are required")
	}
	parts, err := s.RPCParts()
	if err != nil {
		add("%v", err)
	}
	if _, err := os.Stat(s.API.Proto); err != nil {
		add("api.proto: %v", err)
	} else if err == nil && parts.Method != "" {
		b, _ := os.ReadFile(s.API.Proto)
		if !regexp.MustCompile(`rpc\s+` + parts.Method + `\s*\(`).Match(b) {
			add("api.proto: rpc %s not found in %s", parts.Method, s.API.Proto)
		}
	}
	switch s.API.RiskLevel {
	case "low", "medium", "high":
	default:
		add("api.risk_level must be low/medium/high")
	}

	q := s.Datasource.Query
	if q.Name == "" || q.SQL == "" {
		add("datasource.query.name / sql are required")
	}
	switch q.Cardinality {
	case "one", "many", "exec":
	default:
		add("datasource.query.cardinality must be one/many/exec")
	}
	if s.Datasource.Access == "read_only" {
		if writeSQL.MatchString(q.SQL) {
			add("datasource.query.sql: write statement in a read_only datasource")
		}
		if q.Cardinality == "exec" {
			add("datasource.query.cardinality: exec is not allowed for read_only")
		}
	}
	if len(s.Datasource.Tables) == 0 {
		add("datasource.tables: at least one table must be declared")
	}
	for _, t := range s.Datasource.Tables {
		if !regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(t.Name) + `\b`).MatchString(q.SQL) {
			add("datasource.tables: %s is declared but not used in sql", t.Name)
		}
	}

	if len(s.Behavior) == 0 {
		add("behavior: at least one entry is required")
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
	for table := range s.Acceptance.Fixtures {
		if !s.declares(table) {
			add("acceptance.fixtures: table %s is not declared in datasource.tables", table)
		}
	}

	switch s.PatternKind() {
	case "custom":
		if len(s.Generation.AIMayEdit) == 0 {
			add("generation.ai_may_edit: required for pattern custom")
		}
	case "get_by_id":
		if q.Cardinality != "one" {
			add("pattern get_by_id: datasource.query.cardinality must be one")
		}
		if s.Pattern.NotFoundMessage == "" {
			add("pattern get_by_id: not_found_message is required")
		}
	case "list_keyset":
		if q.Cardinality != "many" {
			add("pattern list_keyset: datasource.query.cardinality must be many")
		}
		if len(s.Pattern.OrderBy) == 0 {
			add("pattern list_keyset: order_by is required (last column must be unique, e.g. id)")
		}
		if s.Pattern.Direction != "asc" && s.Pattern.Direction != "desc" {
			add("pattern list_keyset: direction must be asc or desc")
		}
		if ps := s.Pattern.PageSize; ps.Default <= 0 || ps.Max < ps.Default {
			add("pattern list_keyset: page_size.default > 0 and max >= default are required")
		}
		if regexp.MustCompile(`(?i)\b(order\s+by|limit|offset)\b`).MatchString(q.SQL) {
			add("pattern list_keyset: sql must not contain ORDER BY / LIMIT / OFFSET (the factory adds them)")
		}
	default:
		add("pattern.kind must be custom / get_by_id / list_keyset")
	}
	if s.PatternKind() != "custom" && len(s.Generation.AIMayEdit) > 0 {
		add("generation.ai_may_edit: must be empty when the factory generates the handler (pattern %s)", s.PatternKind())
	}
	for _, p := range s.Generation.AIMayEdit {
		if !strings.HasPrefix(p, "internal/handler/") {
			add("generation.ai_may_edit: %s is outside internal/handler/", p)
		}
	}
	return errs
}

func (s *Spec) declares(table string) bool {
	for _, t := range s.Datasource.Tables {
		if t.Name == table {
			return true
		}
	}
	return false
}
