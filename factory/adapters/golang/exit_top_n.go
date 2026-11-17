package golang

// 出口「前N条」的 Go handler: 行的列表,不检查超限(取前 N 条本来就是截断)。
func init() {
	handlerGens["top_n"] = func(g *genCtx) (string, error) {
		return genRowsList(g, g.api.ExitParams["items_field"].(string), 0)
	}
}
