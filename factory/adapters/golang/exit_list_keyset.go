package golang

// 出口「一览」(keyset 分页)的 Go handler。
func init() { handlerGens["list_keyset"] = genListKeyset }
