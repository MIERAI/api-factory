// Package guard 是路径闸门: AI 生成后,检查改动是否都在式样书允许的范围内。
package guard

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"apifactory/factory/spec"
)

// AI 写的代码里禁止出现的 import: 数据访问必须走 sqlc 生成的 Queries。
var forbiddenImports = []string{
	`"database/sql"`,
	`"github.com/jackc/pgx`,
	`"os/exec"`,
	`"net/http"`,
	`"unsafe"`,
}

var sqlLiteral = regexp.MustCompile("(?i)[\"`]\\s*(select|insert|update|delete)\\s")

// Check 对比 git 工作区相对于最近一次提交的改动。
func Check(specs []*spec.Spec) ([]string, []string, error) {
	changed, err := changedFiles()
	if err != nil {
		return nil, nil, err
	}
	var allowed []string
	for _, s := range specs {
		allowed = append(allowed, s.Generation.AIMayEdit...)
	}

	var violations []string
	for _, f := range changed {
		if !matchAny(allowed, f) {
			violations = append(violations, fmt.Sprintf("%s: outside generation.ai_may_edit", f))
			continue
		}
		if !strings.HasSuffix(f, ".go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 删除文件
			}
			return nil, nil, err
		}
		src := string(b)
		for _, imp := range forbiddenImports {
			if strings.Contains(src, imp) {
				violations = append(violations, fmt.Sprintf("%s: forbidden import %s", f, imp))
			}
		}
		if !strings.HasSuffix(f, "_test.go") && sqlLiteral.MatchString(src) {
			violations = append(violations, fmt.Sprintf("%s: raw SQL literal (queries must come from spec)", f))
		}
	}
	return changed, violations, nil
}

func changedFiles() ([]string, error) {
	out, err := exec.Command("git", "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 { // rename
			path = path[i+4:]
		}
		files = append(files, path)
	}
	return files, nil
}

func matchAny(patterns []string, f string) bool {
	for _, p := range patterns {
		if ok, _ := doublestar.Match(p, f); ok {
			return true
		}
	}
	return false
}
