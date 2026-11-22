// Package proj: 工厂目录(home)和项目目录(project)。
//
// 工厂一直以"当前目录 = 仓库根"为前提: design/、specs/、db/schema、gen/、internal/handler、.factory/ 都是相对路径。
// 第二个项目(另一个仓库、另一个数据库)就让工厂在启动时切换到那个项目的根目录(factory --project DIR ...),
// 这些相对路径自然就指向项目自己的目录,不用逐个改。
//
// 切换之后要注意的只有工厂自己的资源:
//   - factory.yaml: 先读工厂的,再用项目根目录的 factory.yaml(有的话)覆盖(按键深度合并)。
//     项目通常只写项目相关的键(proto 的包名前缀等),AI 模型、适配器沿用工厂的。
//   - 适配器目录(清单、prompt 模板): 相对路径按工厂目录解析(HomePath)。
//   - 生成代码的模块名: 读项目根目录 go.mod 的 module 行(Module),不另外配置。
//
// 没有调用 Setup 的时候(单元测试),home 为空: HomePath 原样返回,Config 只读当前目录的 factory.yaml —— 和以前一样。
package proj

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

var home string

// Setup: 记下工厂目录(当前目录),project 非空时切换到项目根目录。只在 main 里调用一次。
func Setup(project string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	home = wd
	if project == "" {
		return nil
	}
	abs, err := filepath.Abs(project)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("--project %s: not a directory", project)
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return fmt.Errorf("--project %s: no go.mod (a project is a Go module: factory init creates one)", project)
	}
	return os.Chdir(abs)
}

// Home: 工厂目录(Setup 之前为空)。
func Home() string { return home }

// InProject: 当前是不是在另一个项目里(不是工厂仓库自己)。
func InProject() bool {
	if home == "" {
		return false
	}
	wd, err := os.Getwd()
	return err == nil && filepath.Clean(wd) != filepath.Clean(home)
}

// HomePath: 工厂自己的资源的路径(适配器目录等)。相对路径按工厂目录解析。
func HomePath(p string) string {
	if home == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(home, p)
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// Module: 当前项目的 Go 模块名(go.mod 的 module 行)。生成代码的 import 路径都以它开头。
func Module() (string, error) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		return "", fmt.Errorf("go.mod: %w (generated code is placed in the project's Go module)", err)
	}
	m := moduleLine.FindSubmatch(b)
	if m == nil {
		return "", errors.New("go.mod: no module line")
	}
	return string(m[1]), nil
}

// Config: factory.yaml 的内容(YAML)。在项目里 = 工厂的 factory.yaml 被项目的 factory.yaml 按键深度覆盖;
// 各个包照旧对它 yaml.Unmarshal,只取自己的那一节。
func Config() ([]byte, error) {
	base, err := os.ReadFile(HomePath("factory.yaml"))
	if err != nil {
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	if !InProject() {
		return base, nil
	}
	over, err := os.ReadFile("factory.yaml")
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project factory.yaml: %w", err)
	}
	return Merge(base, over)
}

// Merge: over 按键深度覆盖 base(映射递归合并,其他类型整体替换)。
func Merge(base, over []byte) ([]byte, error) {
	var b, o map[string]any
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("factory.yaml: %w", err)
	}
	if err := yaml.Unmarshal(over, &o); err != nil {
		return nil, fmt.Errorf("project factory.yaml: %w", err)
	}
	return yaml.Marshal(merge(b, o))
}

func merge(b, o map[string]any) map[string]any {
	if b == nil {
		b = map[string]any{}
	}
	for k, v := range o {
		bm, ok1 := b[k].(map[string]any)
		om, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			b[k] = merge(bm, om)
			continue
		}
		b[k] = v
	}
	return b
}
