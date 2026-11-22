package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"apifactory/factory/prompts"
	"apifactory/factory/review"
)

// 审查的范围(factory/review/fingerprint.go):
//
//	factory review-key [--model M] [--rev REV] <spec.yaml | design/<领域>/<API>.yaml>
//	    沿用上次审查结果的键(设计书里这个 API 引用到的部分 + prompt + 模型)
//	factory review-changed --base REV [specs-dir]
//	    需要审查的式样书: 设计书的指纹和 REV(PR 的 base)不同的 API。领域.yaml 里加了无关的表 → 不列出

// gitReader: 读 rev 里的文件(路径相对当前目录;--project 时也对)。
func gitReader(rev string) (review.Reader, error) {
	if out, err := exec.Command("git", "rev-parse", "--verify", "--quiet", rev+"^{commit}").Output(); err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("git: 找不到提交 %s", rev)
	}
	cache := map[string][]byte{}
	missing := map[string]bool{}
	return func(p string) ([]byte, error) {
		if b, ok := cache[p]; ok {
			return b, nil
		}
		if missing[p] {
			return nil, fs.ErrNotExist
		}
		spec := rev + ":./" + filepath.ToSlash(p)
		if exec.Command("git", "cat-file", "-e", spec).Run() != nil {
			missing[p] = true
			return nil, fs.ErrNotExist
		}
		b, err := exec.Command("git", "show", spec).Output()
		if err != nil {
			return nil, fmt.Errorf("git show %s: %w", spec, err)
		}
		cache[p] = b
		return b, nil
	}, nil
}

// designOf: 式样书 → 设计书(由设计书生成的才有);参数本身是 design/ 的文件就直接用。
func designOf(path string) (string, error) {
	if strings.HasPrefix(filepath.ToSlash(path), "design/") {
		return path, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return designSource(raw), nil
}

func cmdReviewKey(args []string) error {
	fl := flag.NewFlagSet("review-key", flag.ExitOnError)
	model := fl.String("model", "default", "审查的模型(键的一部分)")
	rev := fl.String("rev", "", "按这个 git 提交里的文件计算(空 = 工作区)")
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		return fmt.Errorf("usage: factory review-key [--model M] [--rev REV] <spec.yaml | design/<领域>/<API>.yaml>")
	}
	read := review.Reader(os.ReadFile)
	if *rev != "" {
		r, err := gitReader(*rev)
		if err != nil {
			return err
		}
		read = r
	}
	d, err := designOf(fl.Arg(0))
	if err != nil {
		return err
	}
	if d == "" { // 手写的式样书(没有设计书): 式样书本身 + prompt + 模型
		raw, err := read(fl.Arg(0))
		if err != nil {
			return err
		}
		fmt.Println(shortSum(append(raw, []byte("== prompt\n"+prompts.Review+"\n== model "+*model+"\n")...)))
		return nil
	}
	k, err := review.Key(d, read, prompts.Review, *model)
	if err != nil {
		return err
	}
	fmt.Println(k)
	return nil
}

func cmdReviewChanged(args []string) error {
	fl := flag.NewFlagSet("review-changed", flag.ExitOnError)
	base := fl.String("base", "origin/main", "和哪个提交比(PR 的 base)")
	verbose := fl.Bool("v", false, "stderr 上也列出不需要审查的 API 和理由")
	_ = fl.Parse(args)
	dir := specsDir(fl.Args())
	old, err := gitReader(*base)
	if err != nil {
		return err
	}
	specs, _ := filepath.Glob(filepath.Join(dir, "*", "*", "spec.yaml"))
	sort.Strings(specs)
	for _, s := range specs {
		d, err := designOf(s)
		if err != nil {
			return err
		}
		if d == "" { // 手写的式样书: 原文和 base 比较
			now, _ := os.ReadFile(s)
			was, err := old(s)
			if errors.Is(err, fs.ErrNotExist) || (err == nil && !bytes.Equal(now, was)) {
				fmt.Println(s)
			} else if err != nil {
				return err
			}
			continue
		}
		cur, err := review.DesignFingerprint(d, os.ReadFile)
		if errors.Is(err, review.ErrNoDesign) {
			continue // 生成物里残留的式样书(设计书已经删了)
		}
		if err != nil {
			return err
		}
		was, err := review.DesignFingerprint(d, old)
		switch {
		case errors.Is(err, review.ErrNoDesign):
			fmt.Println(s) // 新的 API
		case err != nil:
			// base 的设计书解析不了(base 本身坏了): 保守地审查
			fmt.Fprintf(os.Stderr, "  %s: base 的设计书读不了(%v)→ 审查\n", d, err)
			fmt.Println(s)
		case was != cur:
			fmt.Println(s)
		case *verbose:
			fmt.Fprintf(os.Stderr, "  = %s: 审查的输入(设计书和它引用的领域部分)和 %s 相同 → 不审查\n", d, *base)
		}
	}
	return nil
}

func shortSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}
