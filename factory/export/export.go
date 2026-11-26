// Package export 输出 API 清单(factory.api-manifest/v1): 每个 API 的人写的来源(设计书或手写式样书)、
// 业务规则和例子的位置、工厂生成的文件、逃生层(AI / 人写的文件、datasource.sql)、验收文件。
//
// 给外部工具用(文档一致性检查、影响分析等): 生成的部分由工厂的闸门保证和来源一致,外部工具只需要看
// 逃生层和来源里的文字(业务规则)。清单是确定的: 同一个提交总是同样的输出(不含时间、不含绝对路径)。
package export

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"apifactory/factory/adapter"
	"apifactory/factory/proj"
	"apifactory/factory/spec"
)

// Schema 是清单的格式版本。只加字段时不改版本;改字段的意思或删字段时升级。
const Schema = "factory.api-manifest/v1"

// Manifest 是一个项目(仓库根目录或 examples/ 下的一个项目)的 API 清单。
type Manifest struct {
	Schema  string `json:"schema"`
	Adapter string `json:"adapter"`
	// Project: 项目根目录(相对工厂的根目录;"." = 工厂自己)。清单里的路径都相对这个目录。
	Project string `json:"project"`
	APIs    []API  `json:"apis"`
	// HandFiles: 项目里所有手写的服务代码(和 factory stats 同一个定义: datasource.sql 生成的查询也算手写)。
	HandFiles []string `json:"hand_files"`
}

// API 是一个 RPC。
type API struct {
	ID     string `json:"id"`     // catalog.list-category-products
	RPC    string `json:"rpc"`    // factory.catalog.v1.CatalogService/ListCategoryProducts
	Name   string `json:"name"`   // ListCategoryProducts
	Domain string `json:"domain"` // catalog
	Kind   string `json:"kind"`   // 出口的种类(list_all / compute / custom ...)
	// Source: 人写的来源。design = design/<领域>/<API>.yaml(式样书由设计编译器生成);spec = 手写的式样书。
	Source     Loc       `json:"source"`
	SourceKind string    `json:"source_kind"`
	Spec       string    `json:"spec"`
	Rules      []Rule    `json:"rules"`
	Examples   []Example `json:"examples"`
	// Generated: 文件头写着从 Source 或 Spec 生成的文件(工厂的闸门保证它们和来源一致)。
	Generated []string `json:"generated"`
	// Escape: 来源不能完全决定的代码 —— 外部工具要对照来源检查的地方。
	Escape     []Escape `json:"escape"`
	Acceptance string   `json:"acceptance,omitempty"` // 从例子生成的验收(存在时)
}

// Loc 是文件里的位置(行号从 1 开始;0 = 整个文件)。
type Loc struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Rule 是一条业务规则(设计书的 业务规则 / 式样书的 behavior)。
type Rule struct {
	Loc
	When string `json:"when"`
	Then string `json:"then"`
}

// Example 是一个例子(验收用例 / 逻辑样例): 工厂会执行它,所以它描述的行为由验收保证。
type Example struct {
	Loc
	Name string `json:"name"`
	Kind string `json:"kind"` // acceptance | logic
}

// Escape 是逃生层的一个文件。
type Escape struct {
	Loc
	Kind   string `json:"kind"`   // logic(计算的纯函数)| handler(自定义 handler)| test | sql(datasource.sql)
	Exists bool   `json:"exists"` // false: 式样书允许写,但还没写
}

// Build 读 specsDir 下所有式样书,生成清单。工作目录 = 项目根目录(和其他命令一样)。
func Build(specsDir string) (*Manifest, error) {
	specs, err := spec.LoadAll(specsDir)
	if err != nil {
		return nil, err
	}
	m, err := adapter.Load()
	if err != nil {
		return nil, err
	}
	genBy, err := generatedFiles(".")
	if err != nil {
		return nil, err
	}
	escapeSpecs := map[string]bool{}
	for _, s := range specs {
		if s.Datasource.SQL != "" {
			escapeSpecs[slash(s.Path)] = true
		}
	}
	lines, err := proj.CountLines(".", escapeSpecs)
	if err != nil {
		return nil, err
	}
	project := "."
	if wd, err := os.Getwd(); err == nil && proj.Home() != "" {
		if rel, err := filepath.Rel(proj.Home(), wd); err == nil {
			project = slash(rel)
		}
	}
	out := &Manifest{Schema: Schema, Adapter: m.Name, Project: project, HandFiles: nonNil(lines.HandFiles), APIs: []API{}}
	for _, s := range specs {
		a, err := buildAPI(s, m, genBy)
		if err != nil {
			return nil, err
		}
		out.APIs = append(out.APIs, a)
	}
	sort.Slice(out.APIs, func(i, j int) bool { return out.APIs[i].ID < out.APIs[j].ID })
	return out, nil
}

func buildAPI(s *spec.Spec, m *adapter.Manifest, genBy map[string][]string) (API, error) {
	parts, _ := s.RPCParts()
	specPath := slash(s.Path)
	a := API{ID: s.ID(), RPC: s.API.RPC, Name: parts.Method, Domain: parts.Domain(), Kind: s.PatternKind(), Spec: specPath,
		Source: Loc{Path: specPath}, SourceKind: "spec"}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return a, err
	}
	if gen, _, from := proj.Header(b); gen && from != "" {
		a.Source, a.SourceKind = Loc{Path: from}, "design"
	}
	src := b
	if a.SourceKind == "design" {
		if src, err = os.ReadFile(a.Source.Path); err != nil {
			return a, fmt.Errorf("%s: design source: %w", specPath, err)
		}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return a, fmt.Errorf("%s: %w", a.Source.Path, err)
	}
	a.Rules, a.Examples = rulesAndExamples(&root, a.Source.Path)
	if a.SourceKind == "design" { // 手写式样书的来源就是它自己: 不会有"从它生成"以外的混淆
		a.Generated = append(a.Generated, genBy[a.Source.Path]...)
	}
	a.Generated = append(a.Generated, genBy[specPath]...)
	a.Generated = uniqSorted(a.Generated)

	files := m.AIFilesFor(a.Kind, adapter.Vars{ID: a.ID, Domain: a.Domain, Method: parts.Method})
	for i, p := range files {
		kind := "handler"
		switch {
		case strings.HasSuffix(p, "_test.go"):
			kind = "test"
		case a.Kind == "compute" && i == 0:
			kind = "logic"
		}
		matches, _ := doublestar.FilepathGlob(p)
		if len(matches) == 0 {
			a.Escape = append(a.Escape, Escape{Loc: Loc{Path: p}, Kind: kind})
		}
		for _, f := range matches {
			a.Escape = append(a.Escape, Escape{Loc: Loc{Path: slash(f)}, Kind: kind, Exists: true})
		}
	}
	if s.Datasource.SQL != "" {
		a.Escape = append(a.Escape, Escape{Loc: Loc{Path: specPath, Line: sqlLine(b)}, Kind: "sql", Exists: true})
	}
	if a.Escape == nil {
		a.Escape = []Escape{}
	}
	if p := spec.AutoAcceptancePath(a.ID); exists(p) {
		a.Acceptance = p
	}
	return a, nil
}

// rulesAndExamples 按位置读来源的业务规则和例子(设计书是中文键,手写式样书是英文键)。
func rulesAndExamples(root *yaml.Node, path string) ([]Rule, []Example) {
	rules, examples := []Rule{}, []Example{}
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	for _, key := range []string{"业务规则", "behavior"} {
		for _, it := range items(get(doc, key)) {
			rules = append(rules, Rule{Loc: Loc{Path: path, Line: it.Line},
				When: scalar(get(it, "情况"), get(it, "when")), Then: scalar(get(it, "结果"), get(it, "then"))})
		}
	}
	for _, it := range items(get(get(doc, "例子"), "用例")) {
		examples = append(examples, Example{Loc: Loc{Path: path, Line: it.Line}, Name: scalar(get(it, "说明")), Kind: "acceptance"})
	}
	for _, it := range items(get(get(doc, "acceptance"), "cases")) {
		examples = append(examples, Example{Loc: Loc{Path: path, Line: it.Line}, Name: scalar(get(it, "name")), Kind: "acceptance"})
	}
	for _, key := range []string{"逻辑样例", "logic_examples"} {
		for _, it := range items(get(doc, key)) {
			examples = append(examples, Example{Loc: Loc{Path: path, Line: it.Line}, Name: scalar(get(it, "说明"), get(it, "name")), Kind: "logic"})
		}
	}
	return rules, examples
}

// generatedFiles: 来源路径 → 文件头写着"从它生成"的文件。
func generatedFiles(root string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, dir := range []string{"cmd", "internal", "gen", "db", "proto", "specs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return filepath.SkipDir
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			b, err := readHead(p)
			if err != nil {
				return err
			}
			if gen, _, from := proj.Header(b); gen && from != "" {
				rel, _ := filepath.Rel(root, p)
				out[from] = append(out[from], slash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readHead(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 2048)
	n, _ := f.Read(b)
	return b[:n], nil
}

func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func items(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	return n.Content
}

func scalar(ns ...*yaml.Node) string {
	for _, n := range ns {
		if n != nil && n.Kind == yaml.ScalarNode {
			return strings.TrimSpace(n.Value)
		}
	}
	return ""
}

// sqlLine: 手写式样书里 datasource.sql 的行号(找不到 = 0)。
func sqlLine(b []byte) int {
	var root yaml.Node
	if yaml.Unmarshal(b, &root) != nil || len(root.Content) == 0 {
		return 0
	}
	if n := get(get(root.Content[0], "datasource"), "sql"); n != nil {
		return n.Line
	}
	return 0
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func slash(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func uniqSorted(s []string) []string {
	sort.Strings(s)
	out := []string{}
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
