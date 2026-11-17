// Package adapter 读取语言适配器的清单。核心通过清单了解"AI 改哪些文件、怎么检查、用什么 prompt",
// 自己不写死任何语言的知识。
package adapter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/ir"
)

type Manifest struct {
	Dir             string              `yaml:"-"`
	Name            string              `yaml:"name"`
	ManifestVersion string              `yaml:"manifest_version"`
	AIFiles         map[string][]string `yaml:"ai_files"`
	ReferenceFiles  map[string]string   `yaml:"reference_files"`
	Prompts         map[string]string   `yaml:"prompts"`
	Guard           Guard               `yaml:"guard"`
	LogicSignature  string              `yaml:"logic_signature"`
	AITools         string              `yaml:"ai_tools"`
	ProtoOptions    []string            `yaml:"proto_file_options"`
}

type Guard struct {
	ForbiddenImports []string `yaml:"forbidden_imports"`
	RawSQL           string   `yaml:"raw_sql"`
	RawSQLSkipSuffix string   `yaml:"raw_sql_skip_suffix"`
	Pure             struct {
		ForbiddenImports []string `yaml:"forbidden_imports"`
		ForbiddenCalls   string   `yaml:"forbidden_calls"`
	} `yaml:"pure"`
}

// Vars: 占位符的值。
type Vars struct {
	ID, Domain, Method, ProtoFile string // ProtoFile: factory/jobs/v1/jobs.proto(可以为空)
}

func (v Vars) replacer() *strings.Replacer {
	dir := filepath.Dir(v.ProtoFile)
	base := strings.TrimSuffix(filepath.Base(v.ProtoFile), ".proto")
	return strings.NewReplacer(
		"{id}", v.ID, "{domain}", v.Domain, "{method_snake}", snake(v.Method),
		"{proto_dir}", dir, "{proto_base}", base, "{version}", filepath.Base(dir),
	)
}

// Load: 读 factory.yaml 指定的适配器清单。
func Load() (*Manifest, error) {
	var cfg struct {
		Adapter string `yaml:"adapter"`
	}
	b, err := os.ReadFile("factory.yaml")
	if err != nil {
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil { // 只取 adapter,其他节由各自的包读取
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	path := filepath.Join(cfg.Adapter, "manifest.yaml")
	b, err = os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.ManifestVersion != "1" {
		return nil, fmt.Errorf("%s: unsupported manifest_version %q", path, m.ManifestVersion)
	}
	m.Dir = cfg.Adapter
	return &m, nil
}

// AIFilesFor: 这个模式下 AI 负责的文件(模式 API 为空)。
func (m *Manifest) AIFilesFor(pattern string, v Vars) []string {
	r := v.replacer()
	var out []string
	for _, p := range m.AIFiles[pattern] {
		out = append(out, r.Replace(p))
	}
	return out
}

// ProtoFileOptions: 生成 proto 时要加的文件级选项(占位符已替换)。
func (m *Manifest) ProtoFileOptions(v Vars) []string {
	r := v.replacer()
	var out []string
	for _, o := range m.ProtoOptions {
		out = append(out, r.Replace(o))
	}
	return out
}

func (m *Manifest) Reference(name string, v Vars) string {
	return v.replacer().Replace(m.ReferenceFiles[name])
}

// Prompt: 这个模式的 prompt 模板原文。
func (m *Manifest) Prompt(pattern string) (string, error) {
	p, ok := m.Prompts[pattern]
	if !ok {
		return "", fmt.Errorf("adapter %s has no prompt for pattern %s", m.Name, pattern)
	}
	b, err := os.ReadFile(filepath.Join(m.Dir, p))
	return string(b), err
}

func (g Guard) RawSQLRegexp() *regexp.Regexp    { return regexp.MustCompile(g.RawSQL) }
func (g Guard) PureCallsRegexp() *regexp.Regexp { return regexp.MustCompile(g.Pure.ForbiddenCalls) }

func snake(s string) string {
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

// Generator: 适配器的代码生成(IR → 这种语言的代码)。每个适配器在 init 里 Register,
// 核心按清单的 name 选择(factory.yaml 的 adapter → manifest.yaml 的 name),不直接引用任何一个适配器。
type Generator func(plan *ir.Plan) error

var generators = map[string]Generator{}

func Register(name string, g Generator) { generators[name] = g }

// Generate: 用清单指定的适配器生成代码。
func (m *Manifest) Generate(plan *ir.Plan) error {
	g, ok := generators[m.Name]
	if !ok {
		var names []string
		for n := range generators {
			names = append(names, n)
		}
		return fmt.Errorf("适配器 %q 没有登记代码生成(已登记: %v;适配器的包要在 factory/cmd/factory 里 import)", m.Name, names)
	}
	return g(plan)
}
