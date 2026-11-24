package metrics

// 热键监控(api-factory: docs/design/hotspots.md §5)。共享数量的读改写(库存 / 计数器 / 余额)的 API,设计书回答
// 「会不会被很多人同时抢」= 不确定(或者没回答)时,生成的 handler 每次写入记录键和用时;键本身不进标签(基数):
//
//	factory_hotkey_window_writes{api}        gauge     窗口里的写入数(最近的 HotKeyWindow 次)
//	factory_hotkey_top_share{api,top}        gauge     窗口里最热的 1 / 3 个键占的比例
//	factory_hotkey_write_seconds{api}        histogram 一次写入(请求)的用时: 等行锁的时间在里面
//
// 告警规则(工厂生成的 hotkeys.rules.yaml): 前 3 个键 ≥ 80% 且窗口写满 → 建议改成分桶(同时抢: 是)。

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// HotKeyWindow: 每个 API 最近多少次写入算比例。
const HotKeyWindow = 1000

type hotWindow struct {
	ring   []string
	next   int
	full   bool
	counts map[string]int
	dur    *hist
}

var (
	hmu  sync.Mutex
	hots = map[string]*hotWindow{}
)

// HotKey: 一次写入(defer HotKey(api, key, time.Now()): 请求结束时记录)。
func HotKey(api, key string, start time.Time) {
	d := time.Since(start)
	hmu.Lock()
	defer hmu.Unlock()
	w := hots[api]
	if w == nil {
		w = &hotWindow{ring: make([]string, HotKeyWindow), counts: map[string]int{}, dur: &hist{counts: make([]uint64, len(Buckets))}}
		hots[api] = w
	}
	if w.full {
		old := w.ring[w.next]
		if w.counts[old]--; w.counts[old] == 0 {
			delete(w.counts, old)
		}
	}
	w.ring[w.next] = key
	w.counts[key]++
	w.next = (w.next + 1) % HotKeyWindow
	if w.next == 0 {
		w.full = true
	}
	s := d.Seconds()
	for i, b := range Buckets {
		if s <= b {
			w.dur.counts[i]++
			break
		}
	}
	w.dur.sum += s
	w.dur.n++
}

// writeHotKeys: Prometheus 的文本格式(没有记录过 = 什么都不输出)。
func writeHotKeys(w io.Writer) {
	hmu.Lock()
	defer hmu.Unlock()
	if len(hots) == 0 {
		return
	}
	apis := make([]string, 0, len(hots))
	for a := range hots {
		apis = append(apis, a)
	}
	sort.Strings(apis)
	fmt.Fprintln(w, "# HELP factory_hotkey_window_writes Writes in the hot-key window (the last HotKeyWindow writes of the API).")
	fmt.Fprintln(w, "# TYPE factory_hotkey_window_writes gauge")
	for _, a := range apis {
		h := hots[a]
		n := h.next
		if h.full {
			n = HotKeyWindow
		}
		fmt.Fprintf(w, "factory_hotkey_window_writes{api=%q} %d\n", a, n)
	}
	fmt.Fprintln(w, "# HELP factory_hotkey_top_share Share of the window's writes that went to the hottest 1 / 3 keys.")
	fmt.Fprintln(w, "# TYPE factory_hotkey_top_share gauge")
	for _, a := range apis {
		h := hots[a]
		var cs []int
		total := 0
		for _, c := range h.counts {
			cs = append(cs, c)
			total += c
		}
		sort.Sort(sort.Reverse(sort.IntSlice(cs)))
		for _, top := range []int{1, 3} {
			sum := 0
			for i := 0; i < top && i < len(cs); i++ {
				sum += cs[i]
			}
			share := 0.0
			if total > 0 {
				share = float64(sum) / float64(total)
			}
			fmt.Fprintf(w, "factory_hotkey_top_share{api=%q,top=\"%d\"} %g\n", a, top, share)
		}
	}
	fmt.Fprintln(w, "# HELP factory_hotkey_write_seconds Duration of a write of the API (row lock waits included).")
	fmt.Fprintln(w, "# TYPE factory_hotkey_write_seconds histogram")
	for _, a := range apis {
		h := hots[a].dur
		var cum uint64
		for i, b := range Buckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "factory_hotkey_write_seconds_bucket{api=%q,le=%q} %d\n", a, fmt.Sprint(b), cum)
		}
		fmt.Fprintf(w, "factory_hotkey_write_seconds_bucket{api=%q,le=\"+Inf\"} %d\n", a, h.n)
		fmt.Fprintf(w, "factory_hotkey_write_seconds_sum{api=%q} %g\n", a, h.sum)
		fmt.Fprintf(w, "factory_hotkey_write_seconds_count{api=%q} %d\n", a, h.n)
	}
}
