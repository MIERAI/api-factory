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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"apifactory/factory/accept"
	"apifactory/factory/guard"
	"apifactory/factory/prepare"
	"apifactory/factory/prompts"
	"apifactory/factory/protoload"
	"apifactory/factory/spec"
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
	if err := prepare.Run(specs); err != nil {
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
	parts, _ := s.RPCParts()
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	segs := strings.Split(parts.ProtoPackage, ".")
	handlerDir := filepath.Join("internal/handler", parts.Domain())
	protoFile, err := protoFileOf(s)
	if err != nil {
		return err
	}
	protoBase := strings.TrimSuffix(filepath.Base(protoFile), ".proto")
	aiFiles := s.AIMayEdit()
	data := map[string]any{
		"RPC":            s.API.RPC,
		"AIMayEdit":      aiFiles,
		"HandlerDir":     handlerDir,
		"HandlerFile":    aiFiles[0],
		"LogicFile":      aiFiles[0],
		"HandlerGenFile": filepath.Join(handlerDir, spec.Snake(parts.Method)+"_gen.go"),
		"QueryFile":      filepath.Join("gen/db", s.ID()+".sql.go"),
		"PBFile":         filepath.Join("gen", filepath.Join(segs...), protoBase+".pb.go"),
		"ConnectFile":    filepath.Join("gen", filepath.Join(segs...), parts.Domain()+segs[len(segs)-1]+"connect", protoBase+".connect.go"),
		"SpecPath":       args[0],
		"SpecYAML":       strings.TrimSpace(string(raw)),
	}
	domainPath, domainRaw := "(无)", "# 这个领域没有领域层"
	if s.Domain != nil {
		domainPath, domainRaw = s.Domain.Path, strings.TrimSpace(s.Domain.Raw)
	}
	data["DomainPath"], data["Domain"] = domainPath, domainRaw
	tmpl := prompts.Implement
	if s.PatternKind() == "compute" {
		// 签名以 prepare 生成的桩为准,不在两处各写一份
		stub, err := os.ReadFile(aiFiles[0])
		if err != nil {
			return fmt.Errorf("logic stub not found (run factory prepare first): %w", err)
		}
		sig := regexp.MustCompile(`(?m)^func \w+Logic\(.*\) \(.*\)`).Find(stub)
		if sig == nil {
			return fmt.Errorf("cannot find the logic function signature in %s", aiFiles[0])
		}
		data["Signature"] = string(sig)
		tmpl = prompts.Compute
	}
	return template.Must(template.New("").Parse(tmpl)).Execute(os.Stdout, data)
}

func cmdGuard(args []string) error {
	specs, err := loadChecked(specsDir(args))
	if err != nil {
		return err
	}
	changed, violations, err := guard.Check(specs)
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
	server := fs.String("server", "bin/server", "server binary")
	db := fs.String("db", os.Getenv("DATABASE_URL"), "database url")
	_ = fs.Parse(args)
	specs, err := loadChecked(specsDir(fs.Args()))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(".factory", 0o755); err != nil {
		return err
	}
	results, err := accept.Run(context.Background(), *server, *db, specs)
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
	fmt.Println(s.ID())
	for _, p := range s.AIMayEdit() {
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
	schema, err := os.ReadFile("db/schema.sql")
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
