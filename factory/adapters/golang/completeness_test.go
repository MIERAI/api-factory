package golang

// 完备性: 核心的注册表里每一种东西,这个适配器都要有实现。新加一个出口 / 类型时,
// 在这里失败(go test),而不是等到某个 API 用到它、生成代码时才失败。

import (
	"testing"

	"apifactory/factory/exits"
	"apifactory/factory/kinds"
)

func TestEveryExitHasHandler(t *testing.T) {
	for _, e := range exits.All() {
		k := e.Kind
		if k == "custom" { // 自定义: AI 写整个 handler
			continue
		}
		if _, ok := handlerGens[k]; !ok {
			t.Errorf("出口 %s: Go 适配器没有 handler 生成器(adapters/golang/exit_%s.go)", k, k)
		}
	}
}

func TestEveryTypeHasGoType(t *testing.T) {
	for _, k := range kinds.All() {
		if k.Base != "" { // 派生的类型按基本类型
			continue
		}
		if _, ok := goTypes[k.ID]; !ok {
			t.Errorf("类型 %s(%s): Go 适配器没有对应的 Go 类型(adapters/golang/querygen.go goTypes)", k.Name, k.ID)
		}
	}
}
