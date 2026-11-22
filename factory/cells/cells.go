// Package cells 是单元库: 标准单元(standard.yaml,产品自带)+ 项目单元(cells/*.yaml,各公司自己的约定)。
// 单元只能由基本元件(factory/core)组成。
package cells

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/core"
)

//go:embed standard.yaml
var standard []byte

// ProjectDir: 项目自己的单元库(可选)。
const ProjectDir = "cells"

type Library struct {
	Filters  map[string]string              // 筛选单元: 名字 → 定义
	ids      map[string]string              // 筛选单元: 名字 → 内部名
	byID     map[string]string              // 筛选单元: 内部名 → 名字
	Patterns map[string][]map[string]string // 模式: 名字 → [{元件: 部件}]
	Sources  map[string]SourceCell          // 来源单元
}

type SourceCell struct {
	Steps []map[string]string `yaml:"步骤"`
	Valid string              `yaml:"有效"`
}

// filterCell: { id: eq, 定义: "…" }。只写定义(字符串)时 id = 名字(项目自己的单元)。
type filterCell struct {
	ID  string `yaml:"id"`
	Def string `yaml:"定义"`
}

func (c *filterCell) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Def = n.Value
		return nil
	}
	type plain filterCell
	return n.Decode((*plain)(c))
}

type file struct {
	Filters  map[string]filterCell          `yaml:"筛选"`
	Patterns map[string][]map[string]string `yaml:"模式"`
	Sources  map[string]SourceCell          `yaml:"来源"`
}

// Load: 标准单元 + 项目单元。同名 → 报错(不允许悄悄覆盖)。
func Load() (*Library, error) {
	lib := &Library{Filters: map[string]string{}, ids: map[string]string{}, byID: map[string]string{}, Patterns: map[string][]map[string]string{}, Sources: map[string]SourceCell{}}
	if err := lib.add("standard.yaml", standard); err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(ProjectDir, "*.yaml"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if err := lib.add(f, b); err != nil {
			return nil, err
		}
	}
	return lib, nil
}

func (l *Library) add(name string, b []byte) error {
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for k, v := range f.Filters {
		if _, dup := l.Filters[k]; dup {
			return fmt.Errorf("%s: 筛选单元 %s 重复定义", name, k)
		}
		id := v.ID
		if id == "" {
			id = k
		}
		if other, dup := l.byID[id]; dup {
			return fmt.Errorf("%s: 筛选单元 %s 的 id %q 和 %s 重复", name, k, id, other)
		}
		l.Filters[k], l.ids[k], l.byID[id] = v.Def, id, k
	}
	for k, v := range f.Sources {
		if _, dup := l.Sources[k]; dup {
			return fmt.Errorf("%s: 来源单元 %s 重复定义", name, k)
		}
		l.Sources[k] = v
	}
	for k, v := range f.Patterns {
		if _, dup := l.Patterns[k]; dup {
			return fmt.Errorf("%s: 模式 %s 重复定义", name, k)
		}
		l.Patterns[k] = v
	}
	return nil
}

// Filter: 筛选单元 → 表达式。columns = 列名(列 = 列1 = 第一列,列2 = 第二列 …),param = 请求参数名。
// members: 组的列表的筛选(列组 / 值组): 每列一个列表参数(和 columns 一一对应);这时 值 = 第一个(判断有没有传)。
func (l *Library) Filter(name string, columns []string, param string, members ...string) (*core.Expr, error) {
	if n, ok := l.byID[name]; ok { // 内部名也可以
		name = n
	}
	def, ok := l.Filters[name]
	if !ok {
		return nil, fmt.Errorf("筛选单元 %q 不存在(单元库里有: %s)", name, strings.Join(l.names(), " / "))
	}
	used := 0
	e, err := core.ParseExpr(def, func(id string) (*core.Expr, bool) {
		switch id {
		case "列":
			id = "列1"
		case "值":
			if len(members) > 0 {
				return core.Param(members[0]), true
			}
			return core.Param(param), true
		case "列组":
			used = len(columns)
			e := &core.Expr{Op: "tuple_cols"}
			for _, c := range columns {
				e.Args = append(e.Args, core.Col(c))
			}
			return e, true
		case "值组":
			if len(members) != len(columns) {
				return nil, false
			}
			return &core.Expr{Op: "tuple_params", Params: members}, true
		}
		var i int
		if _, err := fmt.Sscanf(id, "列%d", &i); err == nil && i >= 1 && i <= len(columns) {
			used = max(used, i)
			return core.Col(columns[i-1]), true
		}
		return nil, false
	})
	if err != nil {
		return nil, fmt.Errorf("筛选单元 %s(给了 %d 列): %w", name, len(columns), err)
	}
	if used != len(columns) {
		return nil, fmt.Errorf("筛选单元 %s 用 %d 列,给了 %d 列", name, used, len(columns))
	}
	return e, nil
}

// FilterID: 筛选单元的名字(或内部名)→ 内部名。spec / IR 里只用内部名。
func (l *Library) FilterID(name string) (string, bool) {
	if id, ok := l.ids[name]; ok {
		return id, true
	}
	if _, ok := l.byID[name]; ok {
		return name, true
	}
	return "", false
}

// FilterName: 内部名 → 设计语言里的名字。
func (l *Library) FilterName(id string) string {
	if n, ok := l.byID[id]; ok {
		return n
	}
	return id
}

// HasFilter: 有没有这个筛选单元。
func (l *Library) HasFilter(name string) bool { _, ok := l.Filters[name]; return ok }

// FilterNames: 全部筛选单元的名字(报错时列出)。
func (l *Library) FilterNames() []string { return l.names() }

func (l *Library) names() []string {
	var out []string
	for k := range l.Filters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Parts: 模式里 $部件 的实际内容(编译器提供)。
type Parts struct {
	Filters   []*core.Expr     // $筛选
	Output    []core.NamedExpr // $输出列
	Cursor    *core.Expr       // $游标
	Order     []core.SortKey   // $排序
	Rows      string           // $行数(参数名)
	Limit     int              // $上限(常数)
	Group     *core.Step       // $每组(可选: pick_per_key)
	Aggregate *core.Step       // $分组(可选: group)
	Rank      *core.Step       // $排名(可选: rank)
}

// Pattern: 模式 → 关系元件的流水线(不含 scan)。
func (l *Library) Pattern(name string, parts Parts) ([]core.Step, error) {
	def, ok := l.Patterns[name]
	if !ok {
		return nil, fmt.Errorf("模式 %q 不存在", name)
	}
	var steps []core.Step
	for _, item := range def {
		for kind, part := range item {
			from := "模式 " + name + " " + part
			switch kind + " " + part {
			case "filter $筛选":
				for _, f := range parts.Filters {
					steps = append(steps, core.Step{Kind: "filter", Pred: f, From: from})
				}
			case "project $输出列":
				steps = append(steps, core.Step{Kind: "project", Project: parts.Output, From: from})
			case "filter $游标":
				if parts.Cursor == nil {
					return nil, fmt.Errorf("模式 %s 需要 $游标", name)
				}
				steps = append(steps, core.Step{Kind: "filter", Pred: parts.Cursor, From: from})
			case "sort $排序":
				steps = append(steps, core.Step{Kind: "sort", Order: parts.Order, From: from})
			case "limit $行数":
				steps = append(steps, core.Step{Kind: "limit", Param: parts.Rows, From: from})
			case "pick_per_key $每组":
				if parts.Group != nil {
					g := *parts.Group
					g.From = from
					steps = append(steps, g)
				}
			case "group $分组":
				if parts.Aggregate != nil {
					g := *parts.Aggregate
					g.From = from
					steps = append(steps, g)
				}
			case "rank $排名":
				if parts.Rank != nil {
					r := *parts.Rank
					r.From = from
					steps = append(steps, r)
				}
			case "limit $上限":
				steps = append(steps, core.Step{Kind: "limit", N: parts.Limit, From: from})
			default:
				return nil, fmt.Errorf("模式 %s: 不认识的部件 %s: %s", name, kind, part)
			}
		}
	}
	return steps, nil
}

// SourceParts: 来源单元的部件(编译器按领域的 存储 声明提供)。
type SourceParts struct {
	Scan    core.Step             // $原始表
	Expand  []core.NamedExpr      // $展开
	Keys    []string              // $每键最新 的键
	NotNull []string              // $键非空 的键(没有声明 可以为空 的键列;空 = 这一步省略)
	Order   []core.SortKey        // $每键最新 的顺序
	Logical []core.NamedExpr      // $逻辑列
	Names   map[string]*core.Expr // 有效 里可用的名字
}

// Source: 来源单元 → 流水线的开头。
func (l *Library) Source(name string, parts SourceParts) ([]core.Step, error) {
	def, ok := l.Sources[name]
	if !ok {
		return nil, fmt.Errorf("来源单元 %q 不存在", name)
	}
	var steps []core.Step
	for _, item := range def.Steps {
		for kind, part := range item {
			from := "来源 " + name + " " + part
			switch kind + " " + part {
			case "scan $原始表":
				st := parts.Scan
				st.From = from
				steps = append(steps, st)
			case "project $展开":
				steps = append(steps, core.Step{Kind: "project", Project: parts.Expand, From: from})
			case "filter $键非空":
				if len(parts.NotNull) == 0 { // 键列都可以为空
					continue
				}
				var conj []*core.Expr
				for _, k := range parts.NotNull {
					conj = append(conj, core.Not(core.IsNull(core.Col(k))))
				}
				pred := conj[0]
				if len(conj) > 1 {
					pred = core.And(conj...)
				}
				steps = append(steps, core.Step{Kind: "filter", Pred: pred, From: from})
			case "pick_per_key $每键最新":
				steps = append(steps, core.Step{Kind: "pick_per_key", Keys: parts.Keys, Order: parts.Order, From: from})
			case "filter $有效":
				e, err := core.ParseExpr(def.Valid, func(id string) (*core.Expr, bool) { x, ok := parts.Names[id]; return x, ok })
				if err != nil {
					return nil, fmt.Errorf("来源单元 %s 的 有效: %w", name, err)
				}
				steps = append(steps, core.Step{Kind: "filter", Pred: e, From: from})
			case "project $逻辑列":
				steps = append(steps, core.Step{Kind: "project", Project: parts.Logical, From: from})
			default:
				return nil, fmt.Errorf("来源单元 %s: 不认识的部件 %s: %s", name, kind, part)
			}
		}
	}
	return steps, nil
}
