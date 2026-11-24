package design

// 负载与热点(docs/design/hotspots.md): 设计书只问业务事实。
//
//	同时抢: 是 | 否 | 不确定      # 只有共享数量的读改写(按键更新里 列: "列 ± …")才有意义;工厂自己判断该问谁
//	负载:                        # 高级(可选): 专家写的工程数字;大多数设计书不写
//	  QPS: 300
//	  p99: 200ms
//	  键分布: hot:3               # uniform | zipf:s | hot:N
//	  桶数: 16                    # 同时抢: 是 的分桶生产线的桶数

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/spec"
)

var hotAnswers = map[string]string{"是": "yes", "否": "no", "不确定": "unsure"}

// HotAnswerName: yes / no / unsure → 设计书的写法。
func HotAnswerName(v string) string {
	for k, x := range hotAnswers {
		if x == v {
			return k
		}
	}
	return "不确定"
}

var reSkew = regexp.MustCompile(`^(uniform|zipf:[0-9]+(\.[0-9]+)?|hot:[1-9][0-9]*)$`)
var reP99 = regexp.MustCompile(`^[1-9][0-9]*ms$`)

func parseHot(e *errs, root *yaml.Node) (string, *spec.LoadDecl) {
	hot := ""
	if n := get(root, "同时抢"); n != nil {
		v, ok := hotAnswers[strings.TrimSpace(n.Value)]
		if !ok {
			e.add(n.Line, "同时抢 只能是 是 / 否 / 不确定(会不会被很多人同时抢,例如限时抢购)")
		}
		hot = v
	}
	n := get(root, "负载")
	if n == nil {
		return hot, nil
	}
	checkKeys(e, n, "负载", "QPS", "p99", "键分布", "桶数")
	l := &spec.LoadDecl{P99: str(n, "p99"), Skew: str(n, "键分布")}
	if s := str(n, "QPS"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			e.add(n.Line, "负载.QPS 必须是正整数(每秒的请求数)")
		}
		l.QPS = v
	}
	if l.P99 != "" && !reP99.MatchString(l.P99) {
		e.add(n.Line, "负载.p99 写成毫秒(例: 200ms)")
	}
	if l.Skew != "" && !reSkew.MatchString(l.Skew) {
		e.add(n.Line, "负载.键分布 只能是 uniform / zipf:s(s > 1)/ hot:N(N ≥ 1)")
	}
	if strings.HasPrefix(l.Skew, "zipf:") {
		if s, _ := strconv.ParseFloat(strings.TrimPrefix(l.Skew, "zipf:"), 64); s <= 1 {
			e.add(n.Line, "负载.键分布 zipf:s 的 s 要大于 1")
		}
	}
	if s := str(n, "桶数"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 2 || v > 256 {
			e.add(n.Line, "负载.桶数 是 2~256 的整数")
		}
		l.Buckets = v
	}
	return hot, l
}

var _ = fmt.Sprintf
