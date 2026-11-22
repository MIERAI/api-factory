package review

// 一个 API 的审查轮次: 本地和 CI 共用一份历史。
//
// 以前轮次只从 .factory/reviews 的目录推出来: 本地 make review-local 一份、CI(spec-pr 按分支缓存)一份。
// 本地第 3 轮回答过的问题,CI 不知道 —— CI 从第 1 轮开始,换个说法再问;CI 的问题下载到本地回答了,
// 本地旧的轮次(引用旧的规则原文)又被当成"问过、没回答"带回来(2026-11-22/23 实测)。
//
// 现在: 一轮 = Round(目录名 = 轮次的 ID: <时间>-<模型>-<API>[-<标签>],本地和 CI 生成的 ID 不会相同),
// 历史 = 下面两处的并集(按 ID 去重,按时间排序):
//   - .factory/reviews/<ID>/(verdict.json + findings.json): 这台机器跑的、或 review-fetch 从 CI 下载的
//   - 轮次记录 design/<领域>/<API>.审查轮次.jsonl(提交): 一行一轮,factory review-result / review-sync 写入。
//     随回答(X.审查.yaml)一起提交 → CI checkout 就有本地的轮次;CI 的轮次 review-fetch 之后写进来,再提交。
//     只保留"最后一次通过 + 之后的轮次"(收敛规则只看这一段),文件不会一直变长。
//     不进审查的指纹(fingerprint.go 只读 X.yaml / X.审查.yaml / 领域): 提交轮次记录不会让 API 重审。
// 带标签的轮次(review-converge 的实验)不写进轮次记录。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Round: 一轮审查(目录或轮次记录的一行)。
type Round struct {
	ID       string    `json:"id"` // 目录名
	Verdict  Verdict   `json:"verdict"`
	Findings []Finding `json:"findings,omitempty"` // 通过的轮次不记(收敛规则只用最后一次通过之后的轮次的问题)
}

// LedgerPath: 设计书 → 它的轮次记录。
func LedgerPath(designPath string) string {
	return strings.TrimSuffix(designPath, ".yaml") + ".审查轮次.jsonl"
}

// RoundID: 目录名里的 API 和标签(不是轮次的目录 → ok = false)。
func RoundID(id string) (api, model string, ok bool) {
	m := dirRe.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	return m[2], m[1], true
}

// ReadLedger: 轮次记录(不存在 = 空)。
func ReadLedger(path string) ([]Round, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Round
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Round
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Merge: 并集(按 ID;同一个 ID 两边都有时后面的优先),按 ID(= 时间)排序。
func Merge(sets ...[]Round) []Round {
	by := map[string]Round{}
	for _, s := range sets {
		for _, r := range s {
			by[r.ID] = r
		}
	}
	out := make([]Round, 0, len(by))
	for _, r := range by {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID }) // ID 以时间开头(UTC,review.sh)
	return out
}

// Trim: 最后一次通过 + 之后的轮次(通过的轮次去掉问题)。轮次记录只存这一段。
func Trim(rs []Round) []Round {
	start := 0
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Verdict.Passed {
			start = i
			break
		}
	}
	out := append([]Round(nil), rs[start:]...)
	if len(out) > 0 && out[0].Verdict.Passed {
		out[0].Findings = nil
	}
	return out
}

// WriteLedger: 和已有的记录合并后写回(内容不变时不写)。返回是否写了。
func WriteLedger(path string, add ...Round) (bool, error) {
	old, err := ReadLedger(path)
	if err != nil {
		return false, err
	}
	rs := Trim(Merge(old, add))
	var b bytes.Buffer
	for _, r := range rs {
		line, err := json.Marshal(r)
		if err != nil {
			return false, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b.Bytes()) {
		return false, nil
	}
	if len(rs) == 0 {
		return false, nil
	}
	return true, os.WriteFile(path, b.Bytes(), 0o644)
}

// ReadDir: 一个审查目录 → Round(没有 verdict.json 的目录 = 审查没完成,不算)。
func ReadDir(dir string) (Round, error) {
	v, err := ReadVerdict(dir)
	if err != nil {
		return Round{}, err
	}
	r := Round{ID: filepath.Base(dir), Verdict: v}
	if fs, err := ReadFindings(dir); err == nil {
		r.Findings = fs
	}
	return r, nil
}

// Rounds: 这个 API(和标签)的历史 = 审查目录 + 轮次记录(ledger 为空 = 只看目录),按时间顺序。
func Rounds(root, ledger, apiID, label string) ([]Round, error) {
	var dirs []Round
	for _, d := range History(root, apiID, label) {
		if r, err := ReadDir(d); err == nil {
			dirs = append(dirs, r)
		}
	}
	var led []Round
	if ledger != "" && label == "" {
		var err error
		if led, err = ReadLedger(ledger); err != nil {
			return nil, err
		}
		want := apiID
		kept := led[:0]
		for _, r := range led {
			if api, _, ok := RoundID(r.ID); ok && api == want {
				kept = append(kept, r)
			}
		}
		led = kept
	}
	return Merge(led, dirs), nil // 目录优先(同一轮: 目录里是原始的结果)
}
