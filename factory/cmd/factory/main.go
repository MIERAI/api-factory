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
	"strings"
	"text/template"

	"apifactory/factory/accept"
	"apifactory/factory/guard"
	"apifactory/factory/prepare"
	"apifactory/factory/prompts"
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
	if failed {
		return nil, fmt.Errorf("spec check failed")
	}
	return specs, nil
}

func cmdCheck(args []string) error {
	specs, err := loadChecked(specsDir(args))
	if err != nil {
		return err
	}
	for _, s := range specs {
		fmt.Printf("✓ %s (%s)\n", s.API.ID, s.Path)
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
	parts, _ := s.RPCParts()
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	segs := strings.Split(parts.ProtoPackage, ".")
	handlerDir := filepath.Join("internal/handler", parts.Domain())
	data := map[string]any{
		"RPC":         s.API.RPC,
		"AIMayEdit":   s.Generation.AIMayEdit,
		"HandlerDir":  handlerDir,
		"HandlerFile": s.Generation.AIMayEdit[0],
		"QueryFile":   filepath.Join("gen/db", s.API.ID+".sql.go"),
		"ConnectFile": filepath.Join("gen", filepath.Join(segs...), parts.Domain()+segs[len(segs)-1]+"connect", parts.Domain()+".connect.go"),
		"SpecPath":    args[0],
		"SpecYAML":    strings.TrimSpace(string(raw)),
	}
	return template.Must(template.New("").Parse(prompts.Implement)).Execute(os.Stdout, data)
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
	failed := 0
	for _, r := range results {
		mark := "✓"
		if !r.Passed {
			mark = "✗"
			failed++
		}
		fmt.Printf("  %s [%s] %s — %s\n", mark, r.Spec, r.Case, r.Detail)
	}
	if failed > 0 {
		return fmt.Errorf("acceptance failed: %d/%d case(s)", failed, len(results))
	}
	fmt.Printf("✓ acceptance passed (%d case(s))\n", len(results))
	return nil
}
