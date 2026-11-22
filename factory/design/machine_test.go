package design

// 状态机的 DRC(R5 §4.1 / §3.1 / §8 风险 2): 拒绝的每种写法各一条,错误里有原因。
// 不用数据库: 表的列直接给(compiler.Column),领域 / 契约 / API 从临时目录里的设计稿读。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"apifactory/factory/cells"
	"apifactory/factory/compiler"
)

const machDomain = `领域: jobs
说明: 测试
时区: Asia/Tokyo
表:
  tasks:
    说明: 任务
    主键: [id]
    更新方式: 可以更新
    列:
      id: 编号
      title: 标题
      status: { 说明: 状态, 枚举: TaskStatus }
      version: 版本
      attempts: 重发计数
      entered_at: 进入时间
  steps:
    说明: 步骤
    主键: [task_id, n]
    更新方式: 可以更新
    列:
      task_id: 任务
      n: 序号
      what: 内容
枚举:
  TaskStatus:
    说明: 状态
    值:
      WAITING: { 说明: 等待, 数据库值: 1 }
      DONE:    { 说明: 完成, 数据库值: 2 }
      FAILED:  { 说明: 失败, 数据库值: 3 }
      EXTRA:   { 说明: 别的, 数据库值: 4 }
    读到不认识的值: 返回"未指定",并记录到监控指标
`

const machContracts = `消息:
  job.run:
    种类: 命令
    版本: 1
    主体: task_id
    字段:
      task_id: { 类型: 整数64, 必填: 是, 范围: "> 0" }
      steps:
        类型: 列表
        必填: 是
        个数: 1~5
        唯一: [n]
        元素:
          n:    { 类型: 整数32, 范围: 1~5 }
          what: { 类型: 文本, 长度: 1~20 }
  job.done:
    种类: 事件
    版本: 1
    主体: task_id
    字段:
      task_id: { 类型: 整数64, 必填: 是 }
      n:       { 类型: 整数32, 必填: 是 }
  job.failed:
    种类: 事件
    版本: 1
    主体: task_id
    字段:
      task_id: { 类型: 整数64, 必填: 是 }
  task.finished:
    种类: 事件
    版本: 1
    主体: task_id
    字段:
      task_id: { 类型: 整数64, 必填: 是 }
`

const machStart = `API: StartTask
说明: 开始
负责人: t
风险: 低
种类: 组合命令
输入:
  title: { 类型: 文本, 必填: 是, 长度: 1~20 }
操作:
  - 任务: { 新建: tasks, 值: { title: 输入.title, status: WAITING, entered_at: 现在 } }
发出:
  - job.run: { task_id: 任务.id, steps: [] }
输出:
  id: { 说明: 编号, 来自: 任务.id }
`

const machOK = `API: TaskFlow
说明: 任务的流程
负责人: t
风险: 低
种类: 状态机
实体: tasks
状态列: status
进入时间列: entered_at
版本列: version
重发计数列: attempts
开始: StartTask
状态:
  WAITING:
    命令: { job.run: { task_id: 实体.id, steps: 载入.steps } }
    超时: 30 秒
    期限: 10 分钟
    期限后: { 到: FAILED, 发出: [ task.finished: { task_id: 实体.id } ] }
  DONE:   { 终态: 是 }
  FAILED: { 终态: 是 }
载入:
  steps: { 表: steps, 条件: [id = task_id], 排序: [n 小到大], 上限: 5, 列: [n, what] }
转移:
  - { 从: WAITING, 收到: job.done, 守卫: "消息.n = 实体.version", 到: DONE, 发出: [ task.finished: { task_id: 实体.id } ] }
  - { 从: WAITING, 收到: job.failed, 到: FAILED }
`

func machCtx(t *testing.T, machine string, extra ...string) (*ctx, *API) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("领域.yaml", machDomain)
	write(ContractsFile, machContracts)
	write("TaskFlow.yaml", machine)
	start := machStart
	if len(extra) > 0 {
		start = extra[0]
	}
	write("StartTask.yaml", start)
	d, apis, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cs, err := loadContracts(dir, Config{Contracts: filepath.Join(dir, "shared")})
	if err != nil {
		t.Fatalf("contracts: %v", err)
	}
	lib, err := cells.Load()
	if err != nil {
		t.Fatal(err)
	}
	cat := map[string][]compiler.Column{
		"tasks": {{Name: "id", Type: "int8", NotNull: true}, {Name: "title", Type: "text", NotNull: true}, {Name: "status", Type: "int4", NotNull: true},
			{Name: "version", Type: "int4", NotNull: true}, {Name: "attempts", Type: "int4", NotNull: true}, {Name: "entered_at", Type: "timestamptz", NotNull: true}},
		"steps": {{Name: "task_id", Type: "int8", NotNull: true}, {Name: "n", Type: "int4", NotNull: true}, {Name: "what", Type: "text", NotNull: true}},
	}
	tz, _ := time.LoadLocation("Asia/Tokyo")
	lock, _ := loadLock(filepath.Join(dir, "proto.lock.yaml"))
	c := &ctx{lib: lib, cfg: Config{PackagePrefix: "factory", Version: "v1"}, d: d, cat: cat, tz: tz, lock: lock, msgByNm: map[string]*message{},
		pkg: "factory.jobs.v1", service: "JobService", contracts: cs, apis: apis}
	for _, a := range apis {
		if a.Name == "TaskFlow" {
			return c, a
		}
	}
	t.Fatal("no TaskFlow")
	return nil, nil
}

func TestMachineDRC(t *testing.T) {
	c, a := machCtx(t, machOK)
	if _, err := c.compileMachine(a); err != nil {
		t.Fatalf("valid machine: %v", err)
	}
	cases := []struct {
		name, old, new, want string
		start                string
	}{
		{"state is not an enum value", "  DONE:   { 终态: 是 }", "  DONE:   { 终态: 是 }\n  NOPE:   { 终态: 是 }", "不是枚举 TaskStatus 的值名", ""},
		{"waiting state without timeout", "    超时: 30 秒\n    期限: 10 分钟\n    期限后: { 到: FAILED, 发出: [ task.finished: { task_id: 实体.id } ] }\n", "", "要写 超时", ""},
		{"final state with a command", "  DONE:   { 终态: 是 }", "  DONE:   { 终态: 是, 命令: { job.run: { task_id: 实体.id, steps: 载入.steps } } }", "终态不写", ""},
		{"edge out of a final state", "  - { 从: WAITING, 收到: job.failed, 到: FAILED }", "  - { 从: WAITING, 收到: job.failed, 到: FAILED }\n  - { 从: DONE, 收到: job.failed, 到: FAILED }", "终态没有出边", ""},
		{"same (from, on) twice", "  - { 从: WAITING, 收到: job.failed, 到: FAILED }", "  - { 从: WAITING, 收到: job.failed, 到: FAILED }\n  - { 从: WAITING, 收到: job.failed, 到: DONE }", "只能有一条转移", ""},
		{"unreachable state", "  FAILED: { 终态: 是 }", "  FAILED: { 终态: 是 }\n  EXTRA:  { 终态: 是 }", "到不了", ""},
		{"waiting state without a way to a final state", "  - { 从: WAITING, 收到: job.failed, 到: FAILED }\n",
			"  - { 从: WAITING, 收到: job.failed, 到: EXTRA }\n", "", ""},
		{"guard without the entity", `守卫: "消息.n = 实体.version"`, `守卫: "消息.n = 1"`, "守卫要比较 实体.列", ""},
		{"message field in a state's command", "命令: { job.run: { task_id: 实体.id, steps: 载入.steps } }", "命令: { job.run: { task_id: 消息.task_id, steps: 载入.steps } }", "这里没有收到的消息", ""},
		{"load without the column of the contract", "列: [n, what]", "列: [n]", "没有列 what", ""},
		{"event as a command", "命令: { job.run: { task_id: 实体.id, steps: 载入.steps } }", "命令: { task.finished: { task_id: 实体.id } }", "是事件", ""},
		{"no start", "开始: StartTask\n", "", "要写 开始", ""},
		{"start does not emit the command", "", "", "要发出初始状态 WAITING 的命令 job.run", strings.Replace(machStart, "发出:\n  - job.run: { task_id: 任务.id, steps: [] }\n", "", 1)},
		{"start enters a final state", "", "", "要是状态机的等待状态", strings.Replace(machStart, "status: WAITING", "status: DONE", 1)},
		{"another API writes the state column", "", "", "只有状态机 TaskFlow 的转移能写", machStart + "\n# 别的写接口\n"},
	}
	for _, tc := range cases {
		src := machOK
		if tc.old != "" {
			if !strings.Contains(src, tc.old) {
				t.Fatalf("%s: old text not found", tc.name)
			}
			src = strings.Replace(src, tc.old, tc.new, 1)
		}
		if tc.want == "" && tc.name == "waiting state without a way to a final state" {
			tc.want = "到不了任何终态"
		}
		if tc.name == "waiting state without a way to a final state" {
			src = strings.Replace(src, "  DONE:   { 终态: 是 }", "  DONE:   { 终态: 是 }\n  EXTRA:\n    命令: { job.run: { task_id: 实体.id, steps: 载入.steps } }\n    超时: 30 秒", 1)
		}
		start := tc.start
		if tc.name == "another API writes the state column" {
			start = ""
		}
		var c *ctx
		var a *API
		if start != "" {
			c, a = machCtx(t, src, start)
		} else {
			c, a = machCtx(t, src)
		}
		if tc.name == "another API writes the state column" {
			w, err := loadAPIString(t, `API: Touch
说明: 改状态
负责人: t
风险: 低
种类: 组合命令
输入:
  id: { 类型: 整数64, 必填: 是, 范围: "> 0" }
操作:
  - 改: { 更新: tasks, 键: { id: 输入.id }, 设置: { status: DONE }, 找不到时: "no task" }
输出:
  id: { 说明: 编号, 来自: 改.id }
`)
			if err != nil {
				t.Fatal(err)
			}
			c.apis = append(c.apis, w)
		}
		_, err := c.compileMachine(a)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func loadAPIString(t *testing.T, src string) (*API, error) {
	p := filepath.Join(t.TempDir(), "X.yaml")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		return nil, err
	}
	return loadAPI(p)
}

// 锁文件: 部署过的状态没有了 → 错误(带部署前的检查查询);写进 已下线 之后可以。
func TestMachineLock(t *testing.T) {
	c, a := machCtx(t, machOK)
	if _, err := c.compileMachine(a); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(c.d.Dir, "machines.lock.yaml")
	b, err := os.ReadFile(lockPath)
	if err != nil || !strings.Contains(string(b), "WAITING: 1") {
		t.Fatalf("lock: %s %v", b, err)
	}
	// FAILED 改名成 EXTRA(= 删除 FAILED + 新增 EXTRA)
	src := strings.ReplaceAll(machOK, "FAILED", "EXTRA")
	c2, a2 := machCtx(t, src)
	c2.d.Dir = c.d.Dir
	_, err = c2.compileMachine(a2)
	if err == nil || !strings.Contains(err.Error(), "SELECT count(*) FROM tasks WHERE status = 3") {
		t.Fatalf("removed state: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte(strings.Replace(string(b), "TaskFlow:\n", "TaskFlow:\n    已下线: [FAILED]\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	c3, a3 := machCtx(t, src)
	c3.d.Dir = c.d.Dir
	if _, err := c3.compileMachine(a3); err != nil {
		t.Fatalf("retired state: %v", err)
	}
}
