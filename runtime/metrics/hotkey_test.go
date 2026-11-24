package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestHotKeyShares(t *testing.T) {
	hmu.Lock()
	hots = map[string]*hotWindow{}
	hmu.Unlock()
	start := time.Now()
	for i := 0; i < HotKeyWindow+500; i++ { // 窗口满了之后: 只看最近的 HotKeyWindow 次
		k := "cold-" + string(rune('a'+i%20))
		if i >= 500 && i%10 < 8 { // 后 1000 次里 80% 打到 hot
			k = "hot"
		}
		HotKey("x.consume", k, start)
	}
	var b strings.Builder
	writeHotKeys(&b)
	out := b.String()
	for _, want := range []string{
		`factory_hotkey_window_writes{api="x.consume"} 1000`,
		`factory_hotkey_top_share{api="x.consume",top="1"} 0.8`,
		`factory_hotkey_write_seconds_count{api="x.consume"} 1500`,
		`factory_hotkey_write_seconds_bucket{api="x.consume",le="+Inf"} 1500`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("没有 %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"hot"`) {
		t.Error("键本身不能进标签")
	}
}
