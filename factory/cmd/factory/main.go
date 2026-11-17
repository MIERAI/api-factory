// factory 是 API 工厂流水线的命令行入口。
//
//	factory check   [specs-dir]                  式样书闸门
//	factory prepare [specs-dir]                  确定性生成(查询/proto/sqlc/骨架/wiring)
//	factory prompt  <spec.yaml>                  生成交给 AI 的指令
//	factory guard   [specs-dir]                  路径闸门(检查 git 工作区的改动)
//	factory accept  --server bin/server --db URL [specs-dir]  验收执行器
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

	"apifactory/factory/accept"
	"apifactory/factory/acceptgen"
	"apifactory/factory/adapter"
	"apifactory/factory/adapters/golang"
	"apifactory/factory/ai"
	"apifactory/factory/answer"
	"apifactory/factory/compiler"
	"apifactory/factory/constraints"
	"apifactory/factory/design"
	"apifactory/factory/exits"
	"apifactory/factory/guard"
	"apifactory/factory/ir"
	"apifactory/factory/perf"
	"apifactory/factory/prompts"
	"apifactory/factory/protoload"
	"apifactory/factory/review"
	"apifactory/factory/spec"
	"apifactory/factory/testenv"
)

func main() {
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
	case "ai-paths":
		err = cmdAIPaths(args)
	case "review-prompt":
		err = cmdReviewPrompt(args)
	case "review-result":
		err = cmdReviewResult(args)
	case "review-stats":
		err = cmdReviewStats(args)
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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: factory check|prepare|prompt|guard|accept ...")
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
	// ③ Go 适配器: IR → Go 代码
	if err := golang.Run(plan); err != nil {
		return err
	}
	fmt.Printf("✓ prepared %d spec(s)\n", len(specs))
	return nil
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
	query, err := os.ReadFile(queryPath)
	if err != nil {
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
	// 轮次: 同一个 API 上次通过之后连续没通过的审查
	cycle := review.Cycle(review.History(".factory/reviews", s.ID(), *label))
	var answers []string
	if p := design["DesignReviewPath"]; p != "" {
		answers = append(answers, p, filepath.Join(filepath.Dir(p), "领域.审查.yaml"))
	}
	prev, err := review.Previous(cycle, answers...)
	if err != nil {
		return err
	}
	data["Round"], data["Previous"], data["AIWrites"] = len(cycle)+1, prev, s.AIWrites()
	data["RelationForms"], data["ConstraintForms"] = constraints.RelationForms(), constraints.Forms()
	if *out != "" {
		b, _ := json.MarshalIndent(map[string]any{"round": len(cycle) + 1, "previous": prev}, "", "  ")
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
	for _, s := range specs {
		byPattern[s.PatternKind()]++
		if s.PatternKind() == "custom" {
			escHandler++
		}
		if s.Datasource.SQL != "" {
			escSQL++
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
//	factory ai --role implement [--model local] [-- 追加参数...]  < prompt
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
//	factory answer design/jobs/GetJobSummary.yaml F1=2 F3=0   一次性回答(0 = 跳过,"文字" = 自己写)
//	factory answer --list design/jobs/GetJobSummary.yaml      只列出问题
//	--all: 也包括不挡流水线的建议
func cmdAnswer(args []string) error {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	list := fs.Bool("list", false, "只列出问题")
	all := fs.Bool("all", false, "也包括不挡流水线的建议")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: factory answer [--list] [--all] design/<领域>/<API>.yaml [F1=2 ...]")
	}
	designFile := fs.Arg(0)
	domain := filepath.Base(filepath.Dir(designFile))
	apiID := domain + "." + design.Kebab(strings.TrimSuffix(filepath.Base(designFile), ".yaml"))
	dir, findings, err := answer.LatestReview(apiID)
	if err != nil {
		return err
	}
	var todo []answer.Finding
	for _, f := range findings {
		if *all || f.Answerable() {
			todo = append(todo, f)
		}
	}
	if len(todo) == 0 {
		fmt.Printf("✓ %s 没有需要回答的问题(%s)\n", apiID, dir)
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
		fmt.Printf("%s: %d 个问题(来自 %s)。直接回车 = 推荐的选项\n", apiID, len(todo), dir)
		ds = answer.Ask(os.Stdin, os.Stdout, todo, domain)
	}
	touched, err := answer.Write(designFile, ds, dir)
	if err != nil {
		return err
	}
	for _, t := range touched {
		fmt.Printf("✓ 写入 %s\n", t)
	}
	fmt.Printf("下一步: make check(重新编译)→ scripts/review.sh specs/%s/%s/spec.yaml(重新审查)\n", domain, strings.TrimPrefix(apiID, domain+"."))
	return nil
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
		Round    int           `json:"round"`
		Previous []review.Prev `json:"previous"`
	}
	if b, err := os.ReadFile(filepath.Join(dir, "prev.json")); err == nil {
		_ = json.Unmarshal(b, &pj)
	}
	if pj.Round == 0 {
		pj.Round = 1
	}
	review.Resolve(fs, pj.Round)
	v := review.Gate(pj.Round, fs, pj.Previous)
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
	tag := map[string]string{}
	for _, id := range v.Blocking {
		tag[id] = "挡"
	}
	for _, id := range v.Late {
		tag[id] = "迟到"
	}
	for _, id := range v.Repeated {
		tag[id] = "重复"
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
	fmt.Printf("⏱ %ds  第 %d 轮  → 挡流水线 %d 件 / 建议 %d 件 / 迟到 %d 件 / 重复提问 %d 件  (%s/findings.json)\n",
		sec, v.Round, len(v.Blocking), len(v.Advisory), len(v.Late), len(v.Repeated), dir)
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
