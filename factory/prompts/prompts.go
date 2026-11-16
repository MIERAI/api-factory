// Package prompts 存放交给 AI 的指令模板。
package prompts

import _ "embed"

//go:embed implement.md
var Implement string

//go:embed compute.md
var Compute string

//go:embed review.md
var Review string
