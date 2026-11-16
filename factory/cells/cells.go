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
	Patterns map[string][]map[string]string // 模式: 名字 → [{元件: 部件}]
	Sources  map[string]SourceCell          // 来源单元
}

type SourceCell struct {
	Steps []map[string]string `yaml:"步骤"`
	Valid string              `yaml:"有效"`
}

type file struct {
	Filters  map[string]string              `yaml:"筛选"`
	Patterns map[string][]map[string]string `yaml:"模式"`
	Sources  map[string]SourceCell          `yaml:"来源"`
}

// Load: 标准单元 + 项目单元。同名 → 报错(不允许悄悄覆盖)。
func Load() (*Library, error) {
	lib := &Library{Filters: map[string]string{}, Patterns: map[string][]map[string]string{}, Sources: map[string]SourceCell{}}
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
		l.Filters[k] = v
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
func (l *Library) Filter(name string, columns []string, param string) (*core.Expr, error) {
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
			return core.Param(param), true
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
	Filters []*core.Expr     // $筛选
	Output  []core.NamedExpr // $输出列
	Cursor  *core.Expr       // $游标
	Order   []core.SortKey   // $排序
	Rows    string           // $行数(参数名)
	Limit   int              // $上限(常数)
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
	Keys    []string              // $键非空 / $每键最新 的键
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
				var conj []*core.Expr
				for _, k := range parts.Keys {
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
