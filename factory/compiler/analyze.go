package compiler

// 用 sqlc 做 SQL 分析(只用 json 输出: schema 目录 + 每个查询的结果列和参数)。
// 和语言无关: 编译器从不读 sqlc 生成的 Go 代码。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const analyzeDir = ".factory/analyze"

type catColumn struct { // 导出别名: Column
	Name    string
	Type    string // int4 / int8 / text / timestamptz ...
	NotNull bool
}

type catalog struct {
	tables map[string][]catColumn
}

func (c *catalog) column(table, name string) (catColumn, bool) {
	for _, col := range c.tables[table] {
		if col.Name == name {
			return col, true
		}
	}
	return catColumn{}, false
}

type analyzedQuery struct {
	Columns []catColumn
	Params  []catColumn // Name = 参数名(sqlc.arg / sqlc.narg 的名字)
}

type analysis struct {
	catalog *catalog
	queries map[string]analyzedQuery
}

// analyze: 把 queries(名字 → "-- name: X :many\nSQL;")写到临时目录,跑 sqlc,读 json。
func analyze(queries map[string]string) (*analysis, error) {
	qdir := filepath.Join(analyzeDir, "queries")
	_ = os.RemoveAll(analyzeDir)
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return nil, err
	}
	if len(queries) == 0 {
		queries = map[string]string{"_probe": "-- name: FactoryProbe :exec\nSELECT 1;\n"}
	}
	for name, body := range queries {
		if err := os.WriteFile(filepath.Join(qdir, name+".sql"), []byte(body), 0o644); err != nil {
			return nil, err
		}
	}
	cfg := `version: "2"
sql:
  - engine: postgresql
    schema: ../../db/schema
    queries: queries
    gen:
      json:
        out: out
        filename: codegen.json
`
	cfgPath := filepath.Join(analyzeDir, "sqlc.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command("sqlc", "generate", "-f", cfgPath)
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlc analyze: %w\n%s", err, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(analyzeDir, "out", "codegen.json"))
	if err != nil {
		return nil, err
	}
	type col struct {
		Name    string `json:"name"`
		NotNull bool   `json:"not_null"`
		Type    struct {
			Name string `json:"name"`
		} `json:"type"`
	}
	var raw struct {
		Catalog struct {
			DefaultSchema string `json:"default_schema"`
			Schemas       []struct {
				Name   string `json:"name"`
				Tables []struct {
					Rel struct {
						Name string `json:"name"`
					} `json:"rel"`
					Columns []col `json:"columns"`
				} `json:"tables"`
			} `json:"schemas"`
		} `json:"catalog"`
		Queries []struct {
			Name    string `json:"name"`
			Columns []col  `json:"columns"`
			Params  []struct {
				Column col `json:"column"`
			} `json:"params"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	a := &analysis{catalog: &catalog{tables: map[string][]catColumn{}}, queries: map[string]analyzedQuery{}}
	for _, sc := range raw.Catalog.Schemas {
		if sc.Name != raw.Catalog.DefaultSchema {
			continue
		}
		for _, t := range sc.Tables {
			var cols []catColumn
			for _, c := range t.Columns {
				cols = append(cols, catColumn{Name: c.Name, Type: c.Type.Name, NotNull: c.NotNull})
			}
			a.catalog.tables[t.Rel.Name] = cols
		}
	}
	for _, q := range raw.Queries {
		var aq analyzedQuery
		for _, c := range q.Columns {
			aq.Columns = append(aq.Columns, catColumn{Name: c.Name, Type: c.Type.Name, NotNull: c.NotNull})
		}
		for _, p := range q.Params {
			aq.Params = append(aq.Params, catColumn{Name: p.Column.Name, Type: p.Column.Type.Name, NotNull: p.Column.NotNull})
		}
		a.queries[q.Name] = aq
	}
	return a, nil
}

// Catalog: schema 目录(表 → 列),给设计编译器用(中文式样书里不重复写列的类型)。
func Catalog() (map[string][]catColumn, error) {
	a, err := analyze(nil)
	if err != nil {
		return nil, err
	}
	return a.catalog.tables, nil
}

// Column 是导出给其他包用的列信息。
type Column = catColumn
