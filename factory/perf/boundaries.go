package perf

// Per-API known boundaries (the project's perf.yaml): a failure perf finds in an API that the project accepts for now,
// with the reason. Listed APIs report ◇ instead of ✗; an entry whose API no longer fails is reported (⚠) so the list
// does not rot, and an entry naming an API that does not exist is an error.
//
//	boundaries:
//	  sales.get-latest-store-notice: "整数的 JSON 键 …(原因)"

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type boundaryFile struct {
	Boundaries map[string]string `yaml:"boundaries"`
}

// LoadBoundaries: path missing = none.
func LoadBoundaries(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f boundaryFile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for api, why := range f.Boundaries {
		if strings.TrimSpace(why) == "" {
			return nil, fmt.Errorf("%s: %s: a known boundary needs its reason", path, api)
		}
	}
	return f.Boundaries, nil
}

// ApplyBoundaries: listed APIs' failures → known boundaries. Returns an error for entries naming unknown APIs.
func ApplyBoundaries(reports []Report, bs map[string]string) error {
	seen, failed := map[string]bool{}, map[string]bool{}
	for i := range reports {
		r := &reports[i]
		why, ok := bs[r.API]
		if !ok {
			continue
		}
		seen[r.API] = true
		if len(r.Fail) > 0 {
			failed[r.API] = true
			b := strings.Join(r.Fail, "; ") + ": 已知边界(perf.yaml): " + why
			if r.Boundary != "" {
				b = r.Boundary + "; " + b
			}
			r.Boundary, r.Fail = b, nil
		}
	}
	var unknown []string
	for api := range bs {
		if !seen[api] {
			unknown = append(unknown, api)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return fmt.Errorf("perf.yaml: no such API: %s", strings.Join(unknown, ", "))
	}
	for i := range reports {
		r := &reports[i]
		if _, ok := bs[r.API]; ok && !failed[r.API] {
			r.Warn = append(r.Warn, "perf.yaml 里列为已知边界,但现在没有失败: 从 perf.yaml 删除")
			failed[r.API] = true // once per API
		}
	}
	return nil
}
