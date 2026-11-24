package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"apifactory/factory/compiler"
)

// cmdStateMigration: factory state-migration [--from db/schema/zz_factory_….sql] <db/migrations/NNNNN_name.sql>
// 工厂生成的表结构(当前状态表 zz_factory_current.sql〔默认〕/ 分桶 zz_factory_buckets.sql;factory prepare 生成)→ goose 迁移:
// Up = 这个文件(幂等: 建表 + 触发器 + 回填,一个事务),Down = 删触发器 → 写回(分桶: 合计写回表本身)→ 删函数 / 视图 / 表。
// 漂移检查(scripts/ci/migrations-check.sh)保证 Up 和 db/schema 一致。
func cmdStateMigration(args []string) error {
	from := compiler.StateFile
	if len(args) == 3 && args[0] == "--from" {
		from, args = args[1], args[2:]
	}
	if len(args) != 1 || !strings.HasSuffix(args[0], ".sql") {
		return fmt.Errorf("usage: factory state-migration [--from db/schema/zz_factory_….sql] db/migrations/NNNNN_name.sql")
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("%s: %w(先 factory prepare)", from, err)
	}
	src := string(b)
	var down []string
	trig := regexp.MustCompile(`(?m)^CREATE OR REPLACE TRIGGER (\S+) .*? ON (\S+) (?:REFERENCING .*? )?FOR EACH`)
	for _, m := range trig.FindAllStringSubmatch(src, -1) {
		down = append(down, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s;", m[1], m[2]))
	}
	for _, m := range regexp.MustCompile(`(?m)^-- factory:down-restore (.*)$`).FindAllStringSubmatch(src, -1) {
		down = append(down, m[1])
	}
	for _, m := range regexp.MustCompile(`(?m)^CREATE OR REPLACE FUNCTION (\S+)\(\)`).FindAllStringSubmatch(src, -1) {
		down = append(down, fmt.Sprintf("DROP FUNCTION IF EXISTS %s() CASCADE;", m[1]))
	}
	for _, m := range regexp.MustCompile(`(?m)^CREATE OR REPLACE VIEW (\S+) AS`).FindAllStringSubmatch(src, -1) {
		down = append(down, fmt.Sprintf("DROP VIEW IF EXISTS %s;", m[1]))
	}
	for _, m := range regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS (\S+) \(`).FindAllStringSubmatch(src, -1) {
		down = append(down, fmt.Sprintf("DROP TABLE IF EXISTS %s;", m[1]))
	}
	out := fmt.Sprintf("-- 工厂生成的表结构(当前状态表 docs/design/current-state.md / 分桶 docs/design/hotspots.md): %s 的内容(factory state-migration 生成)。\n"+
		"-- 建表 + 触发器 + 回填在一个事务里: 期间原始表的写入等待(不会漏掉回填和触发器之间的写)。大表在低峰执行。\n\n"+
		"-- +goose Up\n-- +goose StatementBegin\n%s-- +goose StatementEnd\n\n-- +goose Down\n-- +goose StatementBegin\n%s\n-- +goose StatementEnd\n",
		from, src, strings.Join(down, "\n"))
	if err := os.MkdirAll(filepath.Dir(args[0]), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(args[0], []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("✓ %s(%d 个对象的 Down)\n", args[0], len(down))
	return nil
}
