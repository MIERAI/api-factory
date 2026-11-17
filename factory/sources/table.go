package sources

import (
	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/spec"
)

// 普通的表: 逻辑表 = 物理表。流水线的开头就是 scan,验收数据原样写入。
func init() {
	Register(&Source{
		Name: "", Kind: "table",
		Steps: func(_ *spec.Storage, _ []string, scan core.Step, _ *cells.Library) ([]core.Step, error) {
			return []core.Step{scan}, nil
		},
	})
}
