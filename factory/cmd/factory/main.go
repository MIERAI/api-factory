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
	"strings"
	"text/template"

	"apifactory/factory/accept"
	"apifactory/factory/acceptgen"
	"apifactory/factory/adapter"
	"apifactory/factory/adapters/golang"
	"apifactory/factory/ai"
	"apifactory/factory/answer"
	"apifactory/factory/compiler"
	"apifactory/factory/design"
	"apifactory/factory/guard"
	"apifactory/factory/ir"
	"apifactory/factory/prompts"
	"apifactory/factory/protoload"
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
	failed, human, auto := 0, 0, 0
	for _, r := range results {
		mark := "✓"
		if !r.Passed {
			mark = "✗"
			failed++
		}
		label := r.Spec
		if r.Suite != "" {
			label += " " + r.Suite
			auto++
		} else {
			human++
		}
		fmt.Printf("  %s [%s] %s — %s\n", mark, label, r.Case, r.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("acceptance failed: %d/%d case(s) (人写 %d / 自动 %d)", failed, len(results), human, auto)
	}
	fmt.Printf("✓ acceptance passed (%d case(s): 人写 %d / 自动 %d)\n", len(results), human, auto)
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
	if len(args) != 1 {
		return fmt.Errorf("usage: factory review-prompt <spec.yaml>")
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
	for _, k := range []string{"get_by_id", "list_keyset", "compute", "custom"} {
		fmt.Printf("  %-12s %d\n", k, byPattern[k])
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
		if *all || f.Blocking() {
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
