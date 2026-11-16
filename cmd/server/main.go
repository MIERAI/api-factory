package main

import "apifactory/platform/server"

// services 定义在 wiring_gen.go(由 factory prepare 根据式样书生成)。
func main() {
	server.Run(services)
}
