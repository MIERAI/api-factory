package exits

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// nodeStr / nodeList: 读 YAML 映射里的值(出口自己的子键用)。
func nodeStr(n *yaml.Node, key string) string {
	if n == nil || n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return strings.TrimSpace(n.Content[i+1].Value)
		}
	}
	return ""
}

func nodeList(n *yaml.Node, key string) []string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			var out []string
			for _, x := range n.Content[i+1].Content {
				out = append(out, strings.TrimSpace(x.Value))
			}
			return out
		}
	}
	return nil
}
