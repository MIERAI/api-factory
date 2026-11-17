package golang

import "os"

// 出口「计算」的 Go handler: 映射 + 查询 + AI 写的纯函数;另外生成计算例子的测试和纯函数的桩(第一次)。
func init() {
	handlerGens["compute"] = func(g *genCtx) (string, error) {
		src, err := genCompute(g)
		if err != nil {
			return "", err
		}
		if err := writeLogicExamples(g); err != nil {
			return "", err
		}
		stub := LogicFile(g.api)
		if _, statErr := os.Stat(stub); os.IsNotExist(statErr) {
			if err := writeGo(stub, []byte(genLogicStub(g))); err != nil {
				return "", err
			}
		}
		return src, nil
	}
}
