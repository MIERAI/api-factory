package accept

// 早停(margin 用): 变异测试只关心"差分用例抓到了几次,够不够 N 次"。
// FACTORY_ACCEPT_STOP_AFTER=N(N > 0)→ 失败的差分用例累计到 N 个就停止整个验收(剩下的用例不执行)。
// 判定不变: 够 N 次的变异照样是"够";不够 N 次的变异从来不会触发早停,所有用例照常执行、计数和以前完全一样。
// 不设(或 0)= 和以前一样全部执行(make accept 等)。
//
// 计数的位置: 差分用例的结果在"确定会输出"的地方经过 diffResult(大多是创建的时候;会被折进别的用例的
// 中间结果不经过,例如 message.go 的 step)。每份式样书执行完核对一次(不早停的时候也核对):
// 结果里每个失败的差分用例都计过数,而且计数 == 个数。不一致 = 有结果没经过 diffResult、或者计过数之后
// 被丢掉了(早停会多数)→ 早停的计数不可信,验收报错(不会悄悄地改变判定)。

import (
	"fmt"
	"os"
	"strconv"
)

type earlyStop struct{}

var early struct {
	limit   int      // 0 = 不早停
	kills   int      // 这一次验收里失败的差分用例(全部式样书累计)
	pending []Result // 当前这份式样书里失败的差分用例(早停时它们还没回到 Run 的结果里)
	stopped bool
}

// EarlyStopped: 这一次验收是不是早停了(cmdAccept 打印用)。
func EarlyStopped() (bool, int) { return early.stopped, early.limit }

func earlyBegin() {
	early.limit, _ = strconv.Atoi(os.Getenv("FACTORY_ACCEPT_STOP_AFTER"))
	early.kills, early.pending, early.stopped = 0, nil, false
}

// diffResult: 差分用例的结果都从这里过(gofmt -r 改写的;新写的差分用例也要用它)。
func diffResult(r Result) Result {
	if r.Suite == "diff" && !r.Passed {
		r.counted = true
		early.pending = append(early.pending, r)
		early.kills++
		if early.limit > 0 && early.kills >= early.limit {
			panic(earlyStop{})
		}
	}
	return r
}

// earlyCheck: 一份式样书执行完。rs = 这份式样书的结果。
func earlyCheck(spec string, rs []Result) error {
	n, uncounted := 0, 0
	for _, r := range rs {
		if r.Suite == "diff" && !r.Passed {
			n++
			if !r.counted {
				uncounted++
			}
		}
	}
	// 结果里的每个失败都计过数,而且个数相等 = 计过数的失败一个都没有被丢掉(早停时数到的都会出现在完整的结果里)
	if uncounted > 0 || n != len(early.pending) {
		return fmt.Errorf("%s: 早停的计数不一致(经过 diffResult 的失败 %d 个,结果里失败的差分用例 %d 个): 差分用例的结果要用 diffResult 创建", spec, len(early.pending), n)
	}
	early.pending = nil
	return nil
}
