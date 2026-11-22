package exits

import "testing"

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int64{"30 秒": 30, "15 分钟": 900, "1 小时": 3600, "2小时": 7200} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0 秒", "1 天", "3 周", "1.5 分钟", "abc"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if FormatDuration(900) != "15 分钟" || FormatDuration(3600) != "1 小时" || FormatDuration(45) != "45 秒" {
		t.Error("FormatDuration")
	}
}
