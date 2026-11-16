// Package guard 是路径闸门: AI 生成后,检查改动是否都在式样书允许的范围内。
package guard

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"apifactory/factory/adapter"
	"apifactory/factory/spec"
)

// Check 对比 git 工作区相对于最近一次提交的改动。
// 允许的路径和代码检查规则都来自适配器清单(核心不知道是哪种语言)。
func Check(specs []*spec.Spec, m *adapter.Manifest) ([]string, []string, error) {
	changed, err := changedFiles()
	if err != nil {
		return nil, nil, err
	}
	var allowed []string
	logicFiles := map[string]bool{}
	for _, s := range specs {
		p, _ := s.RPCParts()
		files := m.AIFilesFor(s.PatternKind(), adapter.Vars{ID: s.ID(), Domain: p.Domain(), Method: p.Method})
		allowed = append(allowed, files...)
		if s.PatternKind() == "compute" && len(files) > 0 {
			logicFiles[files[0]] = true
		}
	}
	g := m.Guard
	rawSQL, pureCalls := g.RawSQLRegexp(), g.PureCallsRegexp()

	var violations []string
	for _, f := range changed {
		if !matchAny(allowed, f) {
			violations = append(violations, fmt.Sprintf("%s: outside the files AI may edit (adapter %s)", f, m.Name))
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
		for _, imp := range g.ForbiddenImports {
			if strings.Contains(src, imp) {
				violations = append(violations, fmt.Sprintf("%s: forbidden import %s", f, imp))
			}
		}
		if logicFiles[f] {
			for _, imp := range g.Pure.ForbiddenImports {
				if strings.Contains(src, imp) {
					violations = append(violations, fmt.Sprintf("%s: logic must be pure, forbidden import %s", f, imp))
				}
			}
			if c := pureCalls.FindString(src); c != "" {
				violations = append(violations, fmt.Sprintf("%s: logic must be pure, forbidden call %q", f, c))
			}
		}
		if (g.RawSQLSkipSuffix == "" || !strings.HasSuffix(f, g.RawSQLSkipSuffix)) && rawSQL.MatchString(src) {
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
