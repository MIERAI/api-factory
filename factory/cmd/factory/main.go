// factory 是 API 工厂流水线的命令行入口。
//
//	factory check   [specs-dir]                  式样书闸门
//	factory prepare [specs-dir]                  确定性生成(查询/proto/sqlc/骨架/wiring)
//	factory prompt  <spec.yaml>                  生成交给 AI 的指令
//	factory guard   [specs-dir]                  路径闸门(检查 git 工作区的改动)
//	factory accept  --server bin/server --db URL [specs-dir]  验收执行器
//	factory system  <dir> [--check]              跨项目的消息契约检查 + 系统级的拓扑清单(<契约目录>/topology.json)
//	factory version                              工厂的版本 + 配套的运行时(github.com/MIERAI/api-factory/runtime)
//	factory upgrade [--project DIR] [--check]    项目的运行时依赖 → 配套的版本(go.mod + import),go mod tidy + build
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"apifactory/factory/accept"
	"apifactory/factory/acceptgen"
	"apifactory/factory/adapter"
	_ "apifactory/factory/adapters/golang" // 适配器: 在 init 里登记(adapter.Register)
	"apifactory/factory/ai"
	"apifactory/factory/answer"
	"apifactory/factory/compiler"
	"apifactory/factory/constraints"
	"apifactory/factory/design"
	"apifactory/factory/exits"
	"apifactory/factory/guard"
	"apifactory/factory/ir"
	"apifactory/factory/perf"
	"apifactory/factory/proj"
	"apifactory/factory/prompts"
	"apifactory/factory/protoload"
	"apifactory/factory/review"
	"apifactory/factory/spec"
	"apifactory/factory/testenv"
	"apifactory/factory/upgrade"
)

func main() {
	// factory [--project DIR] <command> ...: DIR = another project's root (its own design/, db/schema, gen/ …); the
	// factory's own resources (factory.yaml defaults, the adapter) stay here (factory/proj).
	project := ""
	if len(os.Args) > 2 && os.Args[1] == "--project" {
		project = os.Args[2]
		os.Args = append(os.Args[:1], os.Args[3:]...)
	}
	if err := proj.Setup(project); err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "check":
		err = cmdCheck(args)
	case "prepare":
		err = cmdPrepare(args)
	case "prompt":
		err = cmdPrompt(args)
	case "guard":
		err = cmdGuard(args)
	case "accept":
		err = cmdAccept(args)
	case "fixtures":
		err = cmdFixtures(args)
	case "ai-paths":
		err = cmdAIPaths(args)
	case "review-prompt":
		err = cmdReviewPrompt(args)
	case "review-result":
		err = cmdReviewResult(args)
	case "review-stats":
		err = cmdReviewStats(args)
	case "review-key":
		err = cmdReviewKey(args)
	case "review-changed":
		err = cmdReviewChanged(args)
	case "review-sync":
		err = cmdReviewSync(args)
	case "stats":
		err = cmdStats(args)
	case "serve-test":
		err = cmdServeTest(args)
	case "ai":
		err = cmdAI(args)
	case "design":
		err = cmdDesign(args)
	case "answer":
		err = cmdAnswer(args)
	case "perf":
		err = cmdPerf(args)
	case "seed-volume":
		err = cmdSeedVolume(args)
	case "loadgen":
		err = cmdLoadgen(args)
	case "doc":
		err = cmdDoc(args)
	case "init":
		err = cmdInit(args)
	case "system":
		err = cmdSystem(args)
	case "version":
		err = cmdVersion(args)
	case "upgrade":
		err = cmdUpgrade(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: factory [--project DIR] check|prepare|prompt|guard|accept|fixtures|version|upgrade ...")
	os.Exit(2)
}

func specsDir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "specs"
}

func loadChecked(dir string) ([]*spec.Spec, error) {
	specs, err := spec.LoadAll(dir)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no spec.yaml under %s", dir)
	}
	failed := false
	for _, s := range specs {
		if errs := s.Check(); len(errs) > 0 {
			failed = true
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", s.Path, e)
			}
		}
	}
	if !failed {
		files, err := protoload.Load()
		if err != nil {
			return nil, err
		}
		for _, s := range specs {
			p, _ := s.RPCParts()
			if _, err := protoload.FindMethod(files, p.ProtoPackage, p.Service, p.Method); err != nil {
				failed = true
				fmt.Fprintf(os.Stderr, "  %s: api.rpc: %v\n", s.Path, err)
			}
		}
	}
	if failed {
		return nil, fmt.Errorf("spec check failed")
	}
	return specs, nil
}

// protoFileOf 返回 rpc 所在的 proto 文件(相对 buf 模块根,如 factory/jobs/v1/jobs.proto)。
func protoFileOf(s *spec.Spec) (string, error) {
	files, err := protoload.Load()
	if err != nil {
		return "", err
	}
	p, _ := s.RPCParts()
	md, err := protoload.FindMethod(files, p.ProtoPackage, p.Service, p.Method)
	if err != nil {
		return "", err
	}
	return md.ParentFile().Path(), nil
}

// protoModuleRoot: buf.yaml 的 modules[0].path(本仓库是 proto)。
const protoModuleRoot = "proto"

func cmdCheck(args []string) error {
	specs, err := loadChecked(specsDir(args))
	if err != nil {
		return err
	}
	for _, s := range specs {
		fmt.Printf("✓ %s (%s)\n", s.ID(), s.Path)
	}
	return nil
}

func cmdPrepare(args []string) error {
	// 生成的代码按配套的运行时写: 项目的 go.mod 不是配套的版本 → 提醒(factory upgrade)。不挡: 编译会说话
	if proj.InProject() {
		if _, err := upgrade.Run(".", upgrade.Options{Check: true, NoBuild: true}); err != nil && !strings.Contains(err.Error(), "does not require the runtime") {
			fmt.Fprintln(os.Stderr, "⚠", err)
		}
	}
	specs, err := loadChecked(specsDir(args))
	if err != nil {
		return err
	}
	// ① 编译器(语言无关): 式样书 → IR + 最终的 SQL
	plan, err := compiler.Compile(specs)
	if err != nil {
		return err
	}
	if err := compiler.Write(plan, ir.DefaultPath); err != nil {
		return err
	}
	// ② 验收生成(语言无关): IR → gen/acceptance
	if err := acceptgen.Write(plan); err != nil {
		return err
	}
	// ②' API 目录(交付看板的"试一下"): 路径 + 人写的 OK 用例
	if err := writeCatalog(plan, specs); err != nil {
		return err
	}
	// ③ 适配器: IR → 这种语言的代码(factory.yaml 选的适配器;各适配器在 init 里登记)
	m, err := adapter.Load()
	if err != nil {
		return err
	}
	if err := m.Generate(plan); err != nil {
		return err
	}
	// ④ 项目的打包模板(Dockerfile、.dockerignore;factory/proj/packaging.go)。工厂仓库自己的 Dockerfile 是手写的
	if proj.InProject() {
		if err := proj.WritePackaging("."); err != nil {
			return err
		}
		// ⑤ README 的环境变量表(运行时的 server.Env + 这个项目用到的;GAPS G22)
		if err := proj.WriteEnvDoc(".", envUses(plan)); err != nil {
			return err
		}
	}
	fmt.Printf("✓ prepared %d spec(s)\n", len(specs))
	return nil
}

// envUses: 影响环境变量的东西(有没有消息 / 状态机 / 分库的表)。
func envUses(plan *ir.Plan) proj.Uses {
	u := proj.Uses{Shards: len(plan.Shards) > 0}
	for _, a := range plan.APIs {
		c := a.Command
		if c == nil {
			continue
		}
		if c.IsMachine() {
			u.Messaging, u.Machines = true, true
		}
		if c.IsHandler() || len(c.Emits) > 0 {
			u.Messaging = true
		}
	}
	return u
}

func cmdPrompt(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: factory prompt <spec.yaml>")
	}
	s, err := spec.Load(args[0])
	if err != nil {
		return err
	}
	if errs := s.Check(); len(errs) > 0 {
		return fmt.Errorf("spec check failed: %s", strings.Join(errs, "; "))
	}
	if !s.AIWrites() {
		return fmt.Errorf("pattern %s is fully generated; nothing for AI to do", s.PatternKind())
	}
	m, err := adapter.Load()
	if err != nil {
		return err
	}
	// proto 文件等事实来自编译器输出的 IR(prepare 之后才有)
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	parts, _ := s.RPCParts()
	vars := adapter.Vars{ID: s.ID(), Domain: parts.Domain(), Method: parts.Method}
	for _, svc := range plan.Services {
		if svc.ProtoPackage == parts.ProtoPackage && svc.Name == parts.Service {
			vars.ProtoFile = svc.ProtoFile
		}
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	aiFiles := m.AIFilesFor(s.PatternKind(), vars)
	data := map[string]any{
		"RPC":            s.API.RPC,
		"AIMayEdit":      aiFiles,
		"HandlerDir":     m.Reference("handler_dir", vars),
		"HandlerFile":    aiFiles[0],
		"LogicFile":      aiFiles[0],
		"HandlerGenFile": m.Reference("handler_gen", vars),
		"QueryFile":      m.Reference("query", vars),
		"PBFile":         m.Reference("pb", vars),
		"ConnectFile":    m.Reference("connect", vars),
		"SpecPath":       args[0],
		"SpecYAML":       strings.TrimSpace(string(raw)),
	}
	domainPath, domainRaw := "(无)", "# 这个领域没有领域层"
	if s.Domain != nil {
		domainPath, domainRaw = s.Domain.Path, strings.TrimSpace(s.Domain.Raw)
	}
	data["DomainPath"], data["Domain"] = domainPath, domainRaw
	if s.PatternKind() == "compute" {
		// 签名以适配器生成的桩为准,不在两处各写一份
		stub, err := os.ReadFile(aiFiles[0])
		if err != nil {
			return fmt.Errorf("logic stub not found (run factory prepare first): %w", err)
		}
		sig := regexp.MustCompile(m.LogicSignature).Find(stub)
		if sig == nil {
			return fmt.Errorf("cannot find the logic function signature in %s", aiFiles[0])
		}
		data["Signature"] = string(sig)
	}
	tmpl, err := m.Prompt(s.PatternKind())
	if err != nil {
		return err
	}
	return template.Must(template.New("").Parse(tmpl)).Execute(os.Stdout, data)
}

func cmdGuard(args []string) error {
	specs, err := loadChecked(specsDir(args))
	if err != nil {
		return err
	}
	m, err := adapter.Load()
	if err != nil {
		return err
	}
	changed, violations, err := guard.Check(specs, m)
	if err != nil {
		return err
	}
	for _, f := range changed {
		fmt.Printf("  changed: %s\n", f)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", v)
		}
		return fmt.Errorf("guard failed: %d violation(s)", len(violations))
	}
	fmt.Printf("✓ guard passed (%d file(s) changed)\n", len(changed))
	return nil
}

// cmdFixtures: 灌入式样书例子的准备数据(同一张表的多个式样书: 后面的覆盖前面的)。
//
//	factory fixtures --db URL specs/…/spec.yaml …
func cmdFixtures(args []string) error {
	fs := flag.NewFlagSet("fixtures", flag.ExitOnError)
	db := fs.String("db", os.Getenv("DATABASE_URL"), "灌 fixture 的 DB(分库: FACTORY_SHARDS)")
	_ = fs.Parse(args)
	specs, err := loadChecked(specsDir(fs.Args()))
	if err != nil {
		return err
	}
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	if err := accept.LoadFixtures(context.Background(), *db, plan, specs); err != nil {
		return err
	}
	fmt.Printf("✓ fixtures loaded (%d spec(s))\n", len(specs))
	return nil
}

func cmdAccept(args []string) error {
	fs := flag.NewFlagSet("accept", flag.ExitOnError)
	baseURL := fs.String("url", os.Getenv("BASE_URL"), "被测 API 的 URL(factory serve-test 会设置 BASE_URL)")
	db := fs.String("db", os.Getenv("DATABASE_URL"), "灌 fixture 用的 DB")
	_ = fs.Parse(args)
	if *baseURL == "" {
		return fmt.Errorf("accept: --url (or BASE_URL) is required; start the server with factory serve-test")
	}
	specs, err := loadChecked(specsDir(fs.Args()))
	if err != nil {
		return err
	}
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	results, err := accept.Run(context.Background(), *baseURL, *db, plan, specs)
	if err != nil {
		return err
	}
	failed, human, auto, diff := 0, 0, 0, 0
	for _, r := range results {
		mark := "✓"
		if !r.Passed {
			mark = "✗"
			failed++
		}
		label := r.Spec
		switch {
		case r.Suite == "diff":
			label += " 差分"
			diff++
		case r.Suite != "":
			label += " " + r.Suite
			auto++
		default:
			human++
		}
		fmt.Printf("  %s [%s] %s — %s\n", mark, label, r.Case, r.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("acceptance failed: %d/%d case(s) (人写 %d / 自动 %d / 差分 %d)", failed, len(results), human, auto, diff)
	}
	fmt.Printf("✓ acceptance passed (%d case(s): 人写 %d / 自动 %d / 差分 %d)\n", len(results), human, auto, diff)
	return nil
}

// cmdAIPaths 列出式样书允许 AI 编辑的路径(试验脚本用来重置到 AI 动手前的状态)。
func cmdAIPaths(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: factory ai-paths <spec.yaml>")
	}
	s, err := spec.Load(args[0])
	if err != nil {
		return err
	}
	m, err := adapter.Load()
	if err != nil {
		return err
	}
	parts, _ := s.RPCParts()
	fmt.Println(s.ID())
	for _, p := range m.AIFilesFor(s.PatternKind(), adapter.Vars{ID: s.ID(), Domain: parts.Domain(), Method: parts.Method}) {
		fmt.Println(p)
	}
	return nil
}

// cmdReviewPrompt 生成式样书审查用的 prompt(式样书 + proto + schema 全部内嵌,审查员不需要工具)。
func cmdReviewPrompt(args []string) error {
	fs := flag.NewFlagSet("review-prompt", flag.ExitOnError)
	label := fs.String("label", "", "审查的标签(和 review.sh 的第 3 个参数一样;轮次按 API + 标签计算)")
	out := fs.String("out", "", "这一次的审查目录: 写入 prev.json(之前各轮的问题,收敛规则用)")
	_ = fs.Parse(args)
	args = fs.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: factory review-prompt [--label L] [--out DIR] <spec.yaml>")
	}
	s, err := spec.Load(args[0])
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	protoFile, err := protoFileOf(s)
	if err != nil {
		return err
	}
	protoPath := filepath.Join(protoModuleRoot, protoFile)
	protoSrc, err := os.ReadFile(protoPath)
	if err != nil {
		return err
	}
	schema, err := readSchema()
	if err != nil {
		return err
	}
	queryPath := filepath.Join("db/queries", s.ID()+".sql")
	var query []byte
	if s.Pattern.Command != nil { // 命令: 没有查询文件,给审查员看写操作的等价 SQL(运行时 txn 组装的形状)
		queryPath, query = "(命令: 写操作作为数据,运行时在一个事务里执行)", []byte(commandSQL(s))
	} else if query, err = os.ReadFile(queryPath); err != nil {
		return fmt.Errorf("generated query not found (run factory prepare first): %w", err)
	}
	domainPath, domainRaw := "(无)", "# 这个领域没有领域层"
	if s.Domain != nil {
		domainPath, domainRaw = s.Domain.Path, strings.TrimSpace(s.Domain.Raw)
	}
	var coverage []string
	if auto, err := spec.LoadAutoAcceptance(s.ID()); err == nil && auto != nil {
		coverage = auto.Coverage
	}
	// 由中文式样书生成的式样书: 审查员也看设计者写的原文(问题要用设计者的词)
	design := map[string]string{}
	if src := designSource(raw); src != "" {
		readOr := func(p, def string) string {
			b, err := os.ReadFile(p)
			if err != nil {
				return def
			}
			return strings.TrimSpace(string(b))
		}
		reviewPath := strings.TrimSuffix(src, ".yaml") + ".审查.yaml"
		domainPath := filepath.Join(filepath.Dir(src), "领域.yaml")
		design = map[string]string{
			"DesignPath": src, "Design": readOr(src, ""),
			"DesignReviewPath": reviewPath, "DesignReview": readOr(reviewPath, "# 还没有回答过的问题"),
			"DesignDomainPath": domainPath, "DesignDomain": readOr(domainPath, "") + "\n\n# 领域的审查记录\n" + readOr(filepath.Join(filepath.Dir(src), "领域.审查.yaml"), ""),
		}
	}
	data := map[string]any{
		"AutoCoverage": coverage,
		"DomainPath":   domainPath,
		"Domain":       domainRaw,
		"SpecPath":     args[0],
		"SpecYAML":     strings.TrimSpace(string(raw)),
		"ProtoPath":    protoPath,
		"Proto":        strings.TrimSpace(string(protoSrc)),
		"Schema":       strings.TrimSpace(string(schema)),
		"QueryPath":    queryPath,
		"Query":        strings.TrimSpace(string(query)),
	}
	for k, v := range design {
		data[k] = v
	}
	// 轮次: 同一个 API 上次通过之后连续没通过的审查(审查目录 + 提交的轮次记录: 本地和 CI 共用,factory/review/rounds.go)
	ledger := ""
	if p := design["DesignPath"]; p != "" {
		ledger = review.LedgerPath(p)
	}
	hist, err := review.Rounds(".factory/reviews", ledger, s.ID(), *label)
	if err != nil {
		return err
	}
	cycle := review.Cycle(hist)
	var answers []review.Answered
	if p := design["DesignReviewPath"]; p != "" {
		if answers, err = review.LoadAnswers(p, filepath.Join(filepath.Dir(p), "领域.审查.yaml")); err != nil {
			return err
		}
	}
	prev := review.Previous(cycle, answers)
	maxRounds, err := reviewMaxRounds()
	if err != nil {
		return err
	}
	data["Answers"] = answers
	data["Round"], data["Previous"], data["AIWrites"] = len(cycle)+1, prev, s.AIWrites()
	data["Command"] = s.Pattern.Command != nil
	data["RelationForms"], data["ConstraintForms"] = constraints.RelationForms(), constraints.Forms()
	var exitKeys []string
	if ex, ok := exits.ByKind(s.PatternKind()); ok {
		exitKeys = ex.ScalarKeys
	}
	data["ExitKeys"] = exitKeys
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{
			"round": len(cycle) + 1, "previous": prev, "answers": answers, "max_rounds": maxRounds,
			"api": s.ID(), "label": *label, "design": design["DesignPath"],
		}, "", "  ")
		if err := os.WriteFile(filepath.Join(*out, "prev.json"), b, 0o644); err != nil {
			return err
		}
	}
	return template.Must(template.New("").Parse(prompts.Review)).Execute(os.Stdout, data)
}

// cmdStats: 各模式的 API 数和逃生率(用 custom 或 datasource.sql 的比例)。
func cmdStats(args []string) error {
	specs, err := spec.LoadAll(specsDir(args))
	if err != nil {
		return err
	}
	byPattern := map[string]int{}
	escHandler, escSQL := 0, 0
	escapeSpecs := map[string]bool{} // 写了 datasource.sql 的式样书: 从它们生成的查询算手写
	for _, s := range specs {
		byPattern[s.PatternKind()]++
		if s.PatternKind() == "custom" {
			escHandler++
		}
		if s.Datasource.SQL != "" {
			escSQL++
			escapeSpecs[filepath.ToSlash(filepath.Clean(s.Path))] = true
		}
	}
	fmt.Printf("APIs: %d\n", len(specs))
	for _, ex := range exits.All() { // 出口的注册表
		fmt.Printf("  %-12s %-6s %d\n", ex.Kind, ex.Name, byPattern[ex.Kind])
	}
	if len(specs) > 0 {
		fmt.Printf("逃生率: handler(custom) %d/%d = %.0f%%  查询(datasource.sql) %d/%d = %.0f%%\n",
			escHandler, len(specs), 100*float64(escHandler)/float64(len(specs)),
			escSQL, len(specs), 100*float64(escSQL)/float64(len(specs)))
	}
	// 手写占比(按代码行;定义见 factory/proj/lines.go 和 boundaries.md §8.2)
	ls, err := proj.CountLines(".", escapeSpecs)
	if err != nil {
		return err
	}
	cats := make([]string, 0, len(ls.Hand))
	for c := range ls.Hand {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	var parts []string
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s %d", c, ls.Hand[c]))
	}
	fmt.Printf("代码行(非空): 生成 %d (工厂 %d, protoc/sqlc %d)  手写 %d", ls.Generated, ls.Generated-ls.Tools, ls.Tools, ls.HandTotal())
	if len(parts) > 0 {
		fmt.Printf(" (%s)", strings.Join(parts, ", "))
	}
	fmt.Printf("\n手写占比: %d/%d = %.1f%%  (生成只算工厂的: %.1f%%)\n", ls.HandTotal(), ls.HandTotal()+ls.Generated,
		100*ls.HandRatio(), 100*ls.HandRatioFactoryOnly())
	for _, f := range ls.HandFiles {
		fmt.Printf("  手写: %s\n", f)
	}
	return nil
}

// readSchema: db/schema/*.sql 全部拼起来(每个领域一个文件)。
func readSchema() ([]byte, error) {
	files, err := filepath.Glob("db/schema/*.sql")
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out, nil
}

// cmdServeTest: 启动被测服务器(测试 DB 会话关掉索引扫描)→ 设置 BASE_URL → 执行 -- 后面的命令 → 停止。
//
//	factory serve-test --server bin/server --db URL -- factory accept --db URL
func cmdServeTest(args []string) error {
	fs := flag.NewFlagSet("serve-test", flag.ExitOnError)
	server := fs.String("server", "bin/server", "server binary")
	db := fs.String("db", os.Getenv("DATABASE_URL"), "database url")
	_ = fs.Parse(args)
	cmd := fs.Args()
	if len(cmd) == 0 {
		return fmt.Errorf("usage: factory serve-test --server bin/server --db URL -- <command...>")
	}
	if err := os.MkdirAll(".factory", 0o755); err != nil {
		return err
	}
	return testenv.WithServer(context.Background(), *server, *db, ".factory/server.log", cmd)
}

// cmdAI: 调用 AI 的唯一入口。
//
//	factory ai --role implement [--model claude-sonnet] [-- 追加参数...]  < prompt
//
// implement 角色会自动加上适配器清单里的 ai_tools(允许的工具)。
func cmdAI(args []string) error {
	fs := flag.NewFlagSet("ai", flag.ExitOnError)
	role := fs.String("role", "", "review / implement")
	model := fs.String("model", "", "覆盖角色的默认模型(必须在 allowed 里)")
	experiment := fs.Bool("experiment", false, "对比实验用: 允许 allowed 以外的模型(会打印警告)")
	_ = fs.Parse(args)
	extra := fs.Args()
	if *role == "implement" {
		m, err := adapter.Load()
		if err != nil {
			return err
		}
		if m.AITools != "" {
			extra = append([]string{"--allowedTools", m.AITools}, extra...)
		}
	}
	return ai.Run(*role, *model, *experiment, extra, os.Stdin, os.Stdout, os.Stderr)
}

// cmdDesign: 中文式样书(design/<领域>/)→ proto + specs/<领域>/(生成)。
func cmdDesign(args []string) error {
	root := "design"
	if len(args) > 0 {
		root = args[0]
	}
	m, err := adapter.Load()
	if err != nil {
		return err
	}
	dirs, _ := filepath.Glob(filepath.Join(root, "*", "领域.yaml"))
	for _, d := range dirs {
		files, err := design.CompileDir(filepath.Dir(d), m)
		if err != nil {
			return err
		}
		fmt.Printf("✓ %s → %d 个文件\n", filepath.Dir(d), len(files))
	}
	return nil
}

// designSource: 生成的式样书的第一行 "# Code generated by factory from design/x/Y.yaml. DO NOT EDIT." → design/x/Y.yaml
func designSource(specYAML []byte) string {
	first, _, _ := strings.Cut(string(specYAML), "\n")
	const p = "# Code generated by factory from "
	if !strings.HasPrefix(first, p) {
		return ""
	}
	src := strings.TrimSuffix(strings.TrimPrefix(first, p), ". DO NOT EDIT.")
	if !strings.HasPrefix(src, "design/") {
		return ""
	}
	return src
}

// cmdAnswer: 回答审查的问题,写进审查记录。
//
//	factory answer design/jobs/GetJobSummary.yaml            终端里一题一题地问
//	factory answer design/jobs/GetJobSummary.yaml F1=2 F3=0   一次性回答(0 = 跳过,"文字" = 自己写,
//	                                                           @design[:说明] = 已经直接改了设计书,
//	                                                           keep:"现在的行为" = 保持现在的行为、规则写成这段文字,
//	                                                           2:"实际的结果" = 选项 2、规则的结果写成这段文字)
//	factory answer --list design/jobs/GetJobSummary.yaml      只列出问题
//	factory answer examples/x/catalog/design/catalog/X.yaml … 项目的设计书: 自动 --project examples/x/catalog
//	--all: 也包括不挡流水线的建议、已经回答过的问题
func cmdAnswer(args []string) error {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	list := fs.Bool("list", false, "只列出问题")
	all := fs.Bool("all", false, "也包括不挡流水线的建议、已经回答过的问题")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: factory answer [--list] [--all] design/<领域>/<API>.yaml [F1=2 ...]")
	}
	designFile, err := enterProject(fs.Arg(0))
	if err != nil {
		return err
	}
	domain := filepath.Base(filepath.Dir(designFile))
	apiID := domain + "." + design.Kebab(strings.TrimSuffix(filepath.Base(designFile), ".yaml"))
	hist, err := review.Rounds(".factory/reviews", review.LedgerPath(designFile), apiID, "")
	if err != nil {
		return err
	}
	if len(hist) == 0 {
		return fmt.Errorf("没有找到 %s 的审查结果(先运行 make review-local / scripts/review.sh,CI 的结果: make review-fetch)", apiID)
	}
	latest := hist[len(hist)-1]
	findings := linkPrevious(hist)
	answers, err := review.LoadAnswers(strings.TrimSuffix(designFile, ".yaml")+".审查.yaml", filepath.Join(filepath.Dir(designFile), "领域.审查.yaml"))
	if err != nil {
		return err
	}
	repeated := map[string]bool{}
	for _, id := range latest.Verdict.Repeated {
		repeated[id] = true
	}
	var todo []answer.Finding
	for _, f := range findings {
		if *all || (f.Answerable() && !repeated[f.ID] && review.AnsweredBy(answers, review.Finding{Finding: f}, f.Refs...) == "") {
			todo = append(todo, f)
		}
	}
	if len(todo) == 0 {
		fmt.Printf("✓ %s 没有需要回答的问题(%s)\n", apiID, latest.ID)
		return nil
	}
	if *list {
		b, _ := json.MarshalIndent(todo, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	var ds []answer.Decision
	if fs.NArg() > 1 {
		if ds, err = answer.FromArgs(todo, fs.Args()[1:]); err != nil {
			return err
		}
	} else {
		fmt.Printf("%s: %d 个问题(来自 %s)。直接回车 = 推荐的选项\n", apiID, len(todo), latest.ID)
		ds = answer.Ask(os.Stdin, os.Stdout, todo, domain)
	}
	touched, err := answer.Write(designFile, ds, latest.ID)
	if err != nil {
		return err
	}
	for _, t := range touched {
		fmt.Printf("✓ 写入 %s\n", t)
	}
	prefix := "" // 项目里: 路径从仓库根写(和用户输入的一样)
	if proj.InProject() {
		if wd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(proj.Home(), wd); err == nil {
				prefix = filepath.ToSlash(rel) + "/"
			}
		}
	}
	fmt.Printf("下一步: 提交 %s%s 和 %s%s(轮次记录: CI 按它接着算轮次)→ make review-local 或推送\n",
		prefix, strings.TrimSuffix(designFile, ".yaml")+".审查.yaml", prefix, review.LedgerPath(designFile))
	return nil
}

// enterProject: 项目里的设计书(<项目>/design/<领域>/X.yaml,项目 = 有 factory.yaml 和 go.mod 的目录)→ 切换到项目,
// 返回项目里的路径(design/<领域>/X.yaml)。已经是 design/ 开头的路径原样返回。
func enterProject(path string) (string, error) {
	p := filepath.ToSlash(filepath.Clean(path))
	if strings.HasPrefix(p, "design/") {
		return p, nil
	}
	i := strings.LastIndex(p, "/design/")
	if i < 0 {
		return path, nil
	}
	root := p[:i]
	for _, f := range []string{"factory.yaml", "go.mod"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			return path, nil
		}
	}
	if err := proj.Setup(root); err != nil {
		return "", err
	}
	return p[i+1:], nil
}

// linkPrevious: 最新一轮的问题,标上"挡流水线"(verdict.json)、绝对编号(<轮次 ID>#F2,写进回答的 问题编号)、
// 之前轮次的同一个问题(same_as 的链)的原文和编号。下一轮按编号 / 原文判断"已回答"。
func linkPrevious(hist []review.Round) []answer.Finding {
	latest := hist[len(hist)-1]
	cycle := append(review.Cycle(hist[:len(hist)-1]), latest)
	blocks := map[string]bool{}
	for _, id := range latest.Verdict.Blocking {
		blocks[id] = true
	}
	prev := review.Previous(cycle[:len(cycle)-1], nil)
	byRef := map[string]review.Prev{}
	for _, p := range prev {
		byRef[p.Ref] = p
	}
	out := make([]answer.Finding, 0, len(latest.Findings))
	for _, rf := range latest.Findings {
		f := rf.Finding
		f.Blocks = blocks[f.ID]
		f.Refs = review.AbsIDs(cycle, len(cycle)-1, rf)
		seen := map[string]bool{}
		for ref := rf.SameAs; ref != "" && !seen[ref]; {
			seen[ref] = true
			p, ok := byRef[ref]
			if !ok {
				break
			}
			if p.Question != f.Question {
				f.Also = append(f.Also, p.Question)
			}
			if id := review.RefID(cycle, ref); id != "" {
				f.Refs = append(f.Refs, id)
			}
			ref = p.Full.SameAs
		}
		out = append(out, f)
	}
	return out
}

// reviewMaxRounds: factory.yaml 的 review.max_rounds(没写 = review.DefaultMaxRounds;0 = 不封顶)。
func reviewMaxRounds() (int, error) {
	raw, err := proj.Config()
	if err != nil {
		return 0, err
	}
	var c struct {
		Review struct {
			MaxRounds *int `yaml:"max_rounds"`
		} `yaml:"review"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return 0, err
	}
	if c.Review.MaxRounds == nil {
		return review.DefaultMaxRounds, nil
	}
	return *c.Review.MaxRounds, nil
}

// cmdReviewSync: 审查目录(本地跑的 + review-fetch 下载的 CI 的)→ 各 API 的轮次记录(design/<领域>/<API>.审查轮次.jsonl)。
// 提交轮次记录 = CI 也知道这些轮次(factory/review/rounds.go)。
func cmdReviewSync(args []string) error {
	specs, _ := filepath.Glob(filepath.Join(specsDir(args), "*", "*", "spec.yaml"))
	sort.Strings(specs)
	for _, sp := range specs {
		raw, err := os.ReadFile(sp)
		if err != nil {
			return err
		}
		d := designSource(raw)
		if d == "" {
			continue
		}
		if _, err := os.Stat(d); err != nil {
			continue
		}
		s, err := spec.Load(sp)
		if err != nil {
			return err
		}
		var rs []review.Round
		for _, dir := range review.History(".factory/reviews", s.ID(), "") {
			if r, err := review.ReadDir(dir); err == nil {
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			continue
		}
		wrote, err := review.WriteLedger(review.LedgerPath(d), rs...)
		if err != nil {
			return err
		}
		if wrote {
			fmt.Printf("✓ %s\n", review.LedgerPath(d))
		}
	}
	return nil
}

// cmdSeedVolume: 给一个库灌接近生产的数据量(恢复演练 / 迁移排练),和 perf 用同一个生成器(业务语义: 自然键、版本、
// 逻辑删除),所有列都填,--dirty 掺脏数据。会清空 --db 里的表: 必须用 --yes 确认库名。
func cmdSeedVolume(args []string) error {
	fs := flag.NewFlagSet("seed-volume", flag.ExitOnError)
	db := fs.String("db", "", "目标库(表会被清空)")
	rows := fs.Int("rows", 1000000, "每张物理表的行数")
	dirty := fs.Bool("dirty", true, "掺脏数据(空值 / 边界值)")
	yes := fs.String("yes", "", "确认: 写目标库的库名")
	_ = fs.Parse(args)
	cfg, err := pgx.ParseConfig(*db)
	if *db == "" || err != nil {
		return fmt.Errorf("seed-volume: --db is required (%v)", err)
	}
	if *yes != cfg.Database {
		return fmt.Errorf("seed-volume: 会清空 %s@%s:%d/%s 的全部表;确认的话加 --yes %s", cfg.User, cfg.Host, cfg.Port, cfg.Database, cfg.Database)
	}
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	t0 := time.Now()
	if _, err := perf.Run(context.Background(), plan, perf.Options{DB: *db, Rows: *rows, FillOnly: true, Dirty: *dirty}); err != nil {
		return err
	}
	fmt.Printf("✓ %s: 每表 %d 行(脏数据 %v) %s\n", cfg.Database, *rows, *dirty, time.Since(t0).Round(time.Second))
	return nil
}

// cmdLoadgen: 压测用的请求(每个 API --per-api 个,参数从 --db 的真实数据抽样)→ JSON(k6 读)。只读。
func cmdLoadgen(args []string) error {
	fs := flag.NewFlagSet("loadgen", flag.ExitOnError)
	db := fs.String("db", "", "抽样用的库(只读)")
	perAPI := fs.Int("per-api", 40, "每个 API 的请求数")
	out := fs.String("out", "-", "输出文件(- = 标准输出)")
	_ = fs.Parse(args)
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	reqs, err := perf.LoadRequests(context.Background(), plan, *db, *perAPI)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(reqs, "", " ")
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = os.Stdout.Write(b)
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ %d requests (%d APIs) → %s\n", len(reqs), len(plan.APIs), *out)
	return os.WriteFile(*out, b, 0o644)
}

// cmdPerf: 性能闸门(P4)。接近真实的数据量 + EXPLAIN ANALYZE。会清空 --db 里的表,只用专门的性能库。
func cmdPerf(args []string) error {
	fs := flag.NewFlagSet("perf", flag.ExitOnError)
	db := fs.String("db", os.Getenv("PERF_DB_URL"), "性能测试专用的 DB(会被清空)")
	rows := fs.Int("rows", 200000, "每张物理表的行数")
	maxMs := fs.Float64("max-ms", 50, "执行时间的阈值(毫秒)")
	_ = fs.Parse(args)
	if *db == "" {
		return fmt.Errorf("perf: --db (or PERF_DB_URL) is required")
	}
	plan, err := ir.Load(ir.DefaultPath)
	if err != nil {
		return fmt.Errorf("IR not found (run factory prepare first): %w", err)
	}
	reports, err := perf.Run(context.Background(), plan, perf.Options{DB: *db, Rows: *rows, MaxMs: *maxMs})
	if err != nil {
		return err
	}
	fails, warns, bounds := 0, 0, 0
	for _, r := range reports {
		if r.Disabled != "" {
			fmt.Printf("–  %-40s 跳过: %s\n", r.API, r.Disabled)
			continue
		}
		mark := "✓"
		if len(r.Warn) > 0 || r.Boundary != "" {
			mark = "⚠"
			warns++
		}
		if r.Boundary != "" {
			bounds++
		}
		if len(r.Fail) > 0 {
			mark = "✗"
			fails++
		}
		fmt.Printf("%s  %-40s %-6s %7.2fms %5d 行  %s\n", mark, r.API, r.Variant, r.Ms, r.Rows, r.Plan)
		for _, m := range r.Fail {
			fmt.Printf("     ✗ %s\n", m)
		}
		for _, m := range r.Warn {
			fmt.Printf("     ⚠ %s\n", m)
		}
		if r.Boundary != "" {
			fmt.Printf("     ⚠ %s\n", r.Boundary)
		}
	}
	if fails > 0 {
		return fmt.Errorf("perf: %d 个失败, %d 个警告 (每表 %d 行, 阈值 %.0fms)", fails, warns, *rows, *maxMs)
	}
	fmt.Printf("✓ perf passed (%d 个计划, 警告 %d 其中已知边界 %d, 每表 %d 行, 阈值 %.0fms)\n", len(reports), warns, bounds, *rows, *maxMs)
	return nil
}

// cmdReviewResult: 解析审查员的输出 → findings.json;按收敛规则(factory/review)判定 → verdict.json。
// 退出码: 0 = 通过,1 = 有挡流水线的问题,2 = 输出解析失败。
func cmdReviewResult(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: factory review-result <审查目录> <秒>")
	}
	dir := args[0]
	sec, _ := strconv.Atoi(args[1])
	raw, err := os.ReadFile(filepath.Join(dir, "ai.json"))
	if err != nil {
		return err
	}
	var result string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "{") {
			var m struct {
				Result string `json:"result"`
			}
			if json.Unmarshal([]byte(l), &m) == nil {
				result = m.Result
			}
		}
	}
	block := regexp.MustCompile("(?s)```json\\s*(\\{.*?\\})\\s*```").FindAllStringSubmatch(result, -1)
	var body string
	if len(block) > 0 {
		body = block[len(block)-1][1]
	} else if i := strings.Index(result, `{`); i >= 0 {
		body = result[i:]
	}
	var parsed struct {
		Findings []review.Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		fmt.Printf("✗ 审查的输出解析不了(%v);见 %s/ai.json\n", err, dir)
		os.Exit(2)
	}
	fs := parsed.Findings
	var pj struct {
		Round     int               `json:"round"`
		Previous  []review.Prev     `json:"previous"`
		Answers   []review.Answered `json:"answers"`
		MaxRounds int               `json:"max_rounds"`
		Label     string            `json:"label"`
		Design    string            `json:"design"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "prev.json")); err == nil {
		_ = json.Unmarshal(b, &pj)
	}
	if pj.Round == 0 {
		pj.Round = 1
	}
	review.Resolve(fs, pj.Round)
	v := review.GateWith(pj.Round, fs, pj.Previous, review.Context{Answers: pj.Answers, MaxRounds: pj.MaxRounds})
	v.Seconds = sec
	fs = append(fs, v.Carried...)
	for _, x := range []struct {
		name string
		v    any
	}{{"findings.json", fs}, {"verdict.json", v}} {
		b, _ := json.MarshalIndent(x.v, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, x.name), b, 0o644); err != nil {
			return err
		}
	}
	// 轮次记录(提交,本地和 CI 共用): 带标签的实验不写
	if pj.Label == "" && pj.Design != "" {
		if _, err := review.WriteLedger(review.LedgerPath(pj.Design), review.Round{ID: filepath.Base(filepath.Clean(dir)), Verdict: v, Findings: fs}); err != nil {
			return err
		}
	}
	tag := map[string]string{}
	for _, id := range v.Blocking {
		tag[id] = "挡"
	}
	for _, id := range v.Capped {
		tag[id] = fmt.Sprintf("封顶(第 %d 轮以后新的 major 不挡;可以回答)", pj.MaxRounds)
	}
	for _, id := range v.Late {
		tag[id] = "迟到"
	}
	for _, id := range v.Repeated {
		tag[id] = "重复(已经回答: " + v.RepeatedOf[id] + ")"
	}
	for _, c := range v.Carried {
		tag[c.ID] = "挡(之前的轮次问过,还没回答)"
	}
	order := map[string]int{"blocker": 0, "major": 1, "minor": 2}
	sort.SliceStable(fs, func(i, j int) bool { return order[fs[i].Severity] < order[fs[j].Severity] })
	for _, f := range fs {
		extra := ""
		if f.SameAs != "" {
			extra = " = " + f.SameAs
		}
		fmt.Printf("  [%-7s] %s %s @ %s  %s%s\n", f.Severity, f.ID, f.Category, f.Location, tag[f.ID], extra)
		fmt.Printf("            %s\n", f.Question)
		for i, o := range f.Options {
			rec := ""
			if i == f.Recommended {
				rec = "  (推荐)"
			}
			fmt.Printf("              %d) %s%s\n", i+1, o.Label, rec)
		}
	}
	fmt.Printf("⏱ %ds  第 %d 轮  → 挡流水线 %d 件 / 建议 %d 件 / 迟到 %d 件 / 封顶 %d 件 / 重复提问 %d 件  (%s/findings.json)\n",
		sec, v.Round, len(v.Blocking), len(v.Advisory), len(v.Late), len(v.Capped), len(v.Repeated), dir)
	if !v.Passed {
		os.Exit(1)
	}
	return nil
}

// cmdReviewStats: 每个 API 审查几轮才通过(P5 的指标: ≤2 轮)。
func cmdReviewStats(args []string) error {
	fs := flag.NewFlagSet("review-stats", flag.ExitOnError)
	match := fs.String("match", "", "只统计名字里含有这个字符串的(例如 -converge)")
	_ = fs.Parse(args)
	root := ".factory/reviews"
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	type cyc struct {
		rounds, late, repeated int
		passed                 bool
	}
	byAPI := map[string][]*cyc{}
	var names []string
	for _, e := range ents {
		v, err := review.ReadVerdict(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		_, key, _ := strings.Cut(e.Name()[16:], "-") // 去掉时间戳和模型
		if !strings.Contains(key, *match) {
			continue
		}
		cs := byAPI[key]
		if len(cs) == 0 || cs[len(cs)-1].passed || v.Round == 1 {
			cs = append(cs, &cyc{})
			if len(byAPI[key]) == 0 {
				names = append(names, key)
			}
		}
		c := cs[len(cs)-1]
		c.rounds, c.passed = v.Round, v.Passed
		c.late += len(v.Late)
		c.repeated += len(v.Repeated)
		byAPI[key] = cs
	}
	sort.Strings(names)
	hist := map[int]int{}
	total, within := 0, 0
	for _, n := range names {
		for _, c := range byAPI[n] {
			mark := "✓"
			if !c.passed {
				mark = "…"
			} else {
				total++
				hist[c.rounds]++
				if c.rounds <= 2 {
					within++
				}
			}
			fmt.Printf("%s %-50s %d 轮  迟到 %d  重复 %d\n", mark, n, c.rounds, c.late, c.repeated)
		}
	}
	if total > 0 {
		fmt.Printf("通过的周期 %d 个: ≤2 轮 %d 个(%.0f%%)", total, within, 100*float64(within)/float64(total))
		for r := 1; r <= 10; r++ {
			if hist[r] > 0 {
				fmt.Printf("  %d 轮×%d", r, hist[r])
			}
		}
		fmt.Println()
	}
	return nil
}

// cmdDoc: 设计语言的参考(docs/design-language.md)从注册表生成。--check: 不写,只确认是最新的。
func cmdDoc(args []string) error {
	fs := flag.NewFlagSet("doc", flag.ExitOnError)
	check := fs.Bool("check", false, "只检查文档是不是最新的")
	_ = fs.Parse(args)
	const path = "docs/design-language.md"
	ref, err := design.Reference()
	if err != nil {
		return err
	}
	old, _ := os.ReadFile(path)
	if *check {
		if string(old) != ref {
			return fmt.Errorf("%s 不是最新的(注册表改了): 运行 factory doc", path)
		}
		fmt.Printf("✓ %s 是最新的\n", path)
		return nil
	}
	if err := os.WriteFile(path, []byte(ref), 0o644); err != nil {
		return err
	}
	fmt.Printf("✓ 写入 %s\n", path)
	return nil
}

// compositeSQL: 组合命令的等价 SQL(审查用)。输入.x = 请求的值,操作名.列 = 前面的操作的返回值,now() = 请求开始的时刻。
func compositeSQL(c *spec.Command) string {
	var b strings.Builder
	b.WriteString("BEGIN;  -- READ COMMITTED,一个事务: 任何一个操作失败 → ROLLBACK,返回那个操作的错误\n")
	for i, op := range c.Ops {
		fmt.Fprintf(&b, "-- %d. %s\n", i+1, op.Name)
		var sets, where []string
		for _, v := range op.Values {
			sets = append(sets, v.Column+" = "+v.Expr)
		}
		for _, k := range op.Key {
			where = append(where, k.Column+" = "+k.Expr)
		}
		if op.Lock != nil {
			where = append(where, op.Lock.Column+" = "+op.Lock.Expr)
		}
		switch op.Kind {
		case "insert", "insert_idempotent":
			var cols, vals []string
			for _, v := range op.Values {
				cols, vals = append(cols, v.Column), append(vals, v.Expr)
			}
			fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES (%s)", op.Table, strings.Join(cols, ", "), strings.Join(vals, ", "))
			if op.Kind == "insert_idempotent" {
				fmt.Fprintf(&b, " ON CONFLICT (%s) DO NOTHING RETURNING *;\n--    没有插入(键已经有了)→ SELECT * FROM %s WHERE %s = <同样的值>: 返回已有的行,%s.新建 = false;不写、不报错\n",
					strings.Join(op.Conflict, ", "), op.Table, strings.Join(op.Conflict, ", "), op.Name)
				continue
			}
			b.WriteString(" RETURNING *;\n")
		case "update":
			if op.Lock != nil {
				sets = append(sets, op.Lock.Column+" = "+op.Lock.Column+" + 1")
			}
			if op.Cond != "" {
				where = append(where, "("+op.Cond+")")
			}
			fmt.Fprintf(&b, "UPDATE %s SET %s WHERE %s RETURNING *;\n", op.Table, strings.Join(sets, ", "), strings.Join(where, " AND "))
			b.WriteString("--    没有碰到行: 同一个事务里按主键再读 → 不存在 = NOT_FOUND")
			if op.Lock != nil {
				b.WriteString(" / 版本不一致 = ABORTED")
			}
			if op.Cond != "" {
				fmt.Fprintf(&b, " / 条件不成立 = FAILED_PRECONDITION(%s)", op.Unmet)
			}
			b.WriteString("\n")
		case "delete":
			fmt.Fprintf(&b, "DELETE FROM %s WHERE %s RETURNING *;\n--    没有碰到行 → NOT_FOUND / 版本不一致 = ABORTED\n", op.Table, strings.Join(where, " AND "))
		}
	}
	b.WriteString("COMMIT;")
	return b.String()
}

// commandSQL: 命令的写操作的等价 SQL(审查用;实际由运行时 runtime/txn 组装,值都是参数)。
func commandSQL(s *spec.Spec) string {
	c := s.Pattern.Command
	if len(c.Ops) > 0 { // 组合命令: 每个操作一条,一个事务
		return compositeSQL(c)
	}
	t := s.Datasource.Table
	val := func(field string) string { return "<请求的 " + field + ">" }
	var sets, where []string
	for _, w := range c.Write {
		sets = append(sets, w.Column+" = "+val(w.Field))
	}
	for _, x := range c.Set {
		v := x.Value
		if v == spec.NowValue {
			v = "<请求开始的时刻>"
		}
		sets = append(sets, x.Column+" = "+v)
	}
	for _, k := range c.Key {
		where = append(where, k.Column+" = "+val(k.Field))
	}
	if c.Lock != nil {
		where = append(where, c.Lock.Column+" = "+val(c.Lock.Field))
	}
	var b strings.Builder
	switch s.PatternKind() {
	case "insert":
		var cols, vals []string
		for _, x := range sets {
			col, v, _ := strings.Cut(x, " = ")
			cols, vals = append(cols, col), append(vals, v)
		}
		fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES (%s)  -- 可以不传的输入没传时: 不写这一列(数据库的默认值)", t, strings.Join(cols, ", "), strings.Join(vals, ", "))
	case "update":
		if c.Lock != nil {
			sets = append(sets, c.Lock.Column+" = "+c.Lock.Column+" + 1")
		}
		fmt.Fprintf(&b, "UPDATE %s SET %s WHERE %s", t, strings.Join(sets, ", "), strings.Join(where, " AND "))
	case "delete":
		fmt.Fprintf(&b, "DELETE FROM %s WHERE %s", t, strings.Join(where, " AND "))
	}
	b.WriteString(" RETURNING <输出的列>;\n-- 没有碰到行: 同一个事务里按主键再读 → 不存在 = NOT_FOUND(找不到时 的消息),存在 = 版本冲突(ABORTED)")
	return b.String()
}
