// Package try 回答"这些文件改了以后,工厂还接受这个项目吗?哪些用例通过、哪些失败?"(factory try)。
//
// 在基准提交的临时副本里(从不碰调用者的工作区)应用改动,跑设计 → 确定性生成 → 编译 → 单元测试 → 验收
// (自己的 Postgres 容器),结果是确定的 JSON(factory.try-result/v1)。不调用 AI: 需要 AI 才能生成时
// 停在 needs_ai。给外部工具用: 检查例子能不能抓到故意改坏的逃生层文件(变异)、证明设计书的修改提案仍然通过验收。
package try

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

// RequestSchema / ResultSchema: 格式版本。只加字段时不改版本;改字段的意思或删字段时升级。
const (
	RequestSchema = "factory.try-request/v1"
	ResultSchema  = "factory.try-result/v1"
)

// Request 是改动的清单(--changes 的文件)。
type Request struct {
	Schema string `json:"schema"`
	Files  []File `json:"files"`
}

// File 是一个文件的改动: 新的全文(content),或者删除(delete: true)。路径相对项目根目录。
type File struct {
	Path    string  `json:"path"`
	Content *string `json:"content,omitempty"`
	Delete  bool    `json:"delete,omitempty"`
}

// ParseRequest 读取并检查改动的清单。错误 = 调用者的用法错误。
func ParseRequest(r io.Reader) (*Request, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("changes: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("changes: trailing data after the JSON object")
	}
	if req.Schema != RequestSchema {
		return nil, fmt.Errorf("changes: schema must be %q (got %q)", RequestSchema, req.Schema)
	}
	seen := map[string]bool{}
	for i := range req.Files {
		f := &req.Files[i]
		p, err := cleanPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("changes: files[%d]: %w", i, err)
		}
		f.Path = p
		switch {
		case f.Delete && f.Content != nil:
			return nil, fmt.Errorf("changes: files[%d] %s: content and delete are exclusive", i, p)
		case !f.Delete && f.Content == nil:
			return nil, fmt.Errorf("changes: files[%d] %s: content (the full new content) or delete: true is required", i, p)
		}
		if seen[p] {
			return nil, fmt.Errorf("changes: files[%d] %s: listed twice", i, p)
		}
		seen[p] = true
	}
	return &req, nil
}

// Paths: 改动的路径(请求里的顺序)。
func (r *Request) Paths() []string {
	out := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, f.Path)
	}
	return out
}

// cleanPath: 项目里的相对路径(斜杠分隔)。绝对路径、跑出项目的路径、.git 下的路径不行。
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is empty")
	}
	if strings.Contains(p, `\`) {
		return "", fmt.Errorf("%s: use forward slashes", p)
	}
	if path.IsAbs(p) || (len(p) > 1 && p[1] == ':') {
		return "", fmt.Errorf("%s: path must be relative to the project", p)
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s: path leaves the project", p)
	}
	if c == ".git" || strings.HasPrefix(c, ".git/") {
		return "", fmt.Errorf("%s: .git is not part of the project", p)
	}
	return c, nil
}
