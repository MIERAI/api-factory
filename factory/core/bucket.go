package core

// 分桶的生产线(docs/design/hotspots.md §4): 一行的数量列 Q 拆成 N 个桶(桶表),"很多人同时取同一个键"时每个请求只锁一个桶。
//
//	桶表 B(键 + bucket + Q + 按桶记的列),CHECK (Q >= 0): 不超卖由数据库保证
//	视图 V = 表 T 的行,Q = T.Q + 桶的合计(T.Q 一直是 0),按桶记的列 = greatest(T 的, 桶里最大的)
//	V 的 INSTEAD OF UPDATE: d = 新 Q − 旧 Q
//	  d < 0(取 −d): 随机的一个桶 UPDATE … WHERE Q >= −d(热路径,只锁一个桶);不够 → 按桶的顺序锁住这个键的全部桶
//	                (FOR UPDATE),合计 ≥ −d 才从各桶扣,否则不更新(0 行 = 运行时照常判定"条件不成立")
//	  d > 0(还): 加到随机的一个桶
//	  其他列有变化: 写 T(冷路径)。返回的行 = 之后的合计
//	V 的 INSTEAD OF DELETE: 删 T 的行(T 的触发器删桶)
//	T 的触发器: 新建 / SET Q(运维登记、数据修复、旧系统)= 合计设成这个值,平分到 N 个桶,T.Q 置 0(⚖️ H9);
//	  改主键 → 桶跟着改;删除 → 删桶;TRUNCATE → 桶也清空
//
// 参考实现 = 一行的语义(命令工厂的模型): 视图上看到的 Q 和一行的时候一样(顺序执行时逐个相同;差分测试 + 元件认证)。

import (
	"fmt"
	"strings"
)

// BucketSpec: 分桶的一张表。Cols = T 的全部列(顺序 = 表的顺序;视图的列顺序和它相同)。
type BucketSpec struct {
	Table, Column, Type string
	Key                 []string
	View, BucketTable   string
	N                   int
	PerBucket           []string
	Cols                []BucketCol
}

type BucketCol struct{ Name, Type string }

// BucketDDL: 分桶的 DDL(幂等: 对已有的库执行 = 迁移,现在的数量分到桶里)。
func BucketDDL(s BucketSpec) string {
	q := quoteIdent
	T, B, V, Q := q(s.Table), q(s.BucketTable), q(s.View), q(s.Column)
	typ := map[string]string{}
	for _, c := range s.Cols {
		typ[c.Name] = c.Type
	}
	isKey, isPer := map[string]bool{}, map[string]bool{}
	for _, k := range s.Key {
		isKey[k] = true
	}
	for _, c := range s.PerBucket {
		isPer[c] = true
	}
	list := func(xs []string, f func(string) string) string {
		var out []string
		for _, x := range xs {
			out = append(out, f(x))
		}
		return strings.Join(out, ", ")
	}
	match := func(a, b string) string { // a.k = b.k AND …
		var out []string
		for _, k := range s.Key {
			out = append(out, fmt.Sprintf("%s%s = %s%s", a, q(k), b, q(k)))
		}
		return strings.Join(out, " AND ")
	}
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("-- 分桶: %s.%s(%d 个桶)。由工厂生成,不要手改(docs/design/hotspots.md)\n", s.Table, s.Column, s.N)
	var defs []string
	for _, k := range s.Key {
		defs = append(defs, fmt.Sprintf("    %s %s NOT NULL", q(k), typ[k]))
	}
	defs = append(defs, "    bucket int2 NOT NULL", fmt.Sprintf("    %s %s NOT NULL DEFAULT 0 CHECK (%s >= 0)", Q, s.Type, Q))
	for _, c := range s.PerBucket {
		defs = append(defs, fmt.Sprintf("    %s %s", q(c), typ[c]))
	}
	defs = append(defs, fmt.Sprintf("    PRIMARY KEY (%s, bucket)", list(s.Key, q)))
	w("CREATE TABLE IF NOT EXISTS %s (\n%s\n);\n", B, strings.Join(defs, ",\n"))

	// 视图: 列的顺序 = 表的顺序
	var sel, agg []string
	agg = append(agg, fmt.Sprintf("sum(b.%s) AS q", Q))
	for i, c := range s.PerBucket {
		agg = append(agg, fmt.Sprintf("max(b.%s) AS p%d", q(c), i))
	}
	perIdx := map[string]int{}
	for i, c := range s.PerBucket {
		perIdx[c] = i
	}
	for _, c := range s.Cols {
		switch {
		case c.Name == s.Column:
			sel = append(sel, fmt.Sprintf("(t.%s + coalesce(x.q, 0))::%s AS %s", Q, s.Type, Q))
		case isPer[c.Name]:
			sel = append(sel, fmt.Sprintf("greatest(t.%s, x.p%d) AS %s", q(c.Name), perIdx[c.Name], q(c.Name)))
		default:
			sel = append(sel, "t."+q(c.Name))
		}
	}
	w("CREATE OR REPLACE VIEW %s AS\nSELECT %s\nFROM %s AS t\nLEFT JOIN LATERAL (SELECT %s FROM %s AS b WHERE %s) AS x ON TRUE;\n",
		V, strings.Join(sel, ", "), T, strings.Join(agg, ", "), B, match("b.", "t."))

	fn := func(name, body string) {
		w("CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $factory$\n%s\n$factory$;\n", q(name), body)
	}
	keyOf := func(row string) string { return list(s.Key, func(k string) string { return row + "." + q(k) }) }
	newKeyMatch := match("", "NEW.")
	oldKeyMatch := match("", "OLD.")
	perSet := func(row string) string {
		var out []string
		for _, c := range s.PerBucket {
			out = append(out, fmt.Sprintf(", %s = %s.%s", q(c), row, q(c)))
		}
		return strings.Join(out, "")
	}
	// T 的触发器: 写进 T 的数量 = 合计设成这个值
	fn(s.BucketTable+"_set", fmt.Sprintf(`BEGIN
    IF NEW.%[3]s < 0 THEN -- 和一行的时候的 CHECK (数量 >= 0) 一样
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = format('new row for relation %%L violates check constraint (%%s >= 0)', %[9]s, %[10]s);
    END IF;
    DELETE FROM %[1]s WHERE %[2]s;
    INSERT INTO %[1]s (%[4]s, bucket, %[3]s%[5]s)
    SELECT %[6]s, i, NEW.%[3]s / %[7]d + CASE WHEN i < NEW.%[3]s %% %[7]d THEN 1 ELSE 0 END%[8]s
    FROM generate_series(0, %[11]d) AS i;
    NEW.%[3]s := 0;
    RETURN NEW;
END`, B, newKeyMatch, Q, list(s.Key, q), list2(s.PerBucket, q), keyOf("NEW"), s.N, list2(s.PerBucket, func(c string) string { return "NEW." + q(c) }),
		quoteLit(s.Table), quoteLit(s.Column), s.N-1))
	fn(s.BucketTable+"_rekey", fmt.Sprintf(`BEGIN
    IF (%s) IS DISTINCT FROM (%s) THEN
        UPDATE %s SET %s WHERE %s;
    END IF;
    RETURN NEW;
END`, keyOf("NEW"), keyOf("OLD"), B, list(s.Key, func(k string) string { return q(k) + " = NEW." + q(k) }), oldKeyMatch))
	fn(s.BucketTable+"_del", fmt.Sprintf("BEGIN\n    DELETE FROM %s WHERE %s;\n    RETURN OLD;\nEND", B, oldKeyMatch))
	fn(s.BucketTable+"_trunc", fmt.Sprintf("BEGIN\n    TRUNCATE %s;\n    RETURN NULL;\nEND", B))
	// 视图: 取 / 还 / 其他列
	var other, otherSet []string
	for _, c := range s.Cols {
		if c.Name != s.Column && !isKey[c.Name] {
			other = append(other, c.Name)
			otherSet = append(otherSet, fmt.Sprintf("%s = NEW.%s", q(c.Name), q(c.Name)))
		}
	}
	perChanged := "FALSE"
	if len(s.PerBucket) > 0 {
		perChanged = fmt.Sprintf("(%s) IS DISTINCT FROM (%s)", list(s.PerBucket, func(c string) string { return "NEW." + q(c) }), list(s.PerBucket, func(c string) string { return "OLD." + q(c) }))
	}
	coldChanged := "FALSE"
	var cold []string
	for _, c := range other {
		if !isPer[c] {
			cold = append(cold, c)
		}
	}
	if len(cold) > 0 {
		coldChanged = fmt.Sprintf("(%s) IS DISTINCT FROM (%s)", list(cold, func(c string) string { return "NEW." + q(c) }), list(cold, func(c string) string { return "OLD." + q(c) }))
	}
	baseUpdate := "NULL;"
	if len(otherSet) > 0 {
		baseUpdate = fmt.Sprintf("UPDATE %s SET %s WHERE %s;", T, strings.Join(otherSet, ", "), oldKeyMatch)
	}
	keySame := fmt.Sprintf("(%s) IS NOT DISTINCT FROM (%s)", keyOf("NEW"), keyOf("OLD"))
	fn(s.View+"_upd", fmt.Sprintf(`DECLARE
    d %[1]s := NEW.%[2]s - OLD.%[2]s;
    need %[1]s;
    total %[1]s;
    b int2 := floor(random() * %[8]d)::int2; -- 一个请求选一次(写在 WHERE 里会每行重新取随机数)
    v %[1]s;
    bb int2;
    hit bool;
BEGIN
    IF NOT %[3]s THEN
        RAISE EXCEPTION 'factory: 分桶的表 %% 不能改主键', %[4]s;
    END IF;
    IF d < 0 THEN
        -- 热路径: 从随机的桶开始,第一个没被锁、又够的桶(SKIP LOCKED: 不等别人;只锁这一行)。
        -- 不写成 UPDATE … WHERE 量 >= …: 等到别人提交之后条件不成立时,PostgreSQL 仍然留着那一行的锁(EvalPlanQual),会死锁
        -- 子事务: 没取到时回滚它 —— 被别人刚改过、重新检查之后不够的行,PostgreSQL 也会留着锁(EvalPlanQual),
        -- 留着去等回退的互斥就会和回退互相等(死锁)
        hit := true;
        BEGIN
            SELECT bucket INTO bb FROM %[5]s WHERE %[7]s AND %[2]s >= -d ORDER BY (bucket + %[8]d - b) %% %[8]d LIMIT 1 FOR UPDATE SKIP LOCKED;
            IF NOT FOUND THEN
                RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'factory: bucket miss';
            END IF;
            UPDATE %[5]s SET %[2]s = %[2]s + d%[6]s WHERE %[7]s AND bucket = bb;
        EXCEPTION WHEN raise_exception THEN
            hit := false;
        END;
        IF NOT hit THEN
            -- 够的桶都被别人锁着(同时的请求比桶多): 排队等随机的那一个桶(同样在子事务里: 等到之后不够 → 回滚,不留锁)
            hit := true;
            BEGIN
                PERFORM 1 FROM %[5]s WHERE %[7]s AND bucket = b AND %[2]s >= -d FOR UPDATE;
                IF NOT FOUND THEN
                    RAISE EXCEPTION USING ERRCODE = 'P0001', MESSAGE = 'factory: bucket miss';
                END IF;
                UPDATE %[5]s SET %[2]s = %[2]s + d%[6]s WHERE %[7]s AND bucket = b;
            EXCEPTION WHEN raise_exception THEN
                hit := false;
            END;
        END IF;
        IF NOT hit THEN
            -- 回退: 表的这一行当互斥(FOR NO KEY UPDATE: 不挡外键检查;热路径不碰它),再按桶的顺序锁住这个键的全部桶,
            -- 合计够才扣(不够 = 不更新)。热路径的事务只锁一个桶、锁到之后不再等别的;回退一次只有一个 → 没有死锁
            PERFORM 1 FROM %[14]s WHERE %[7]s FOR NO KEY UPDATE;
            SELECT sum(%[2]s) INTO total FROM (SELECT %[2]s FROM %[5]s WHERE %[7]s ORDER BY bucket FOR UPDATE) AS l;
            IF coalesce(total, 0) < -d THEN
                RETURN NULL;
            END IF;
            need := -d;
            FOR i IN 0 .. %[15]d LOOP
                EXIT WHEN need = 0;
                SELECT %[2]s INTO v FROM %[5]s WHERE %[7]s AND bucket = i;
                IF v > 0 THEN
                    UPDATE %[5]s SET %[2]s = %[2]s - least(v, need)%[6]s WHERE %[7]s AND bucket = i;
                    need := need - least(v, need);
                END IF;
            END LOOP;
        END IF;
    ELSIF d > 0 THEN
        UPDATE %[5]s SET %[2]s = %[2]s + d%[6]s WHERE %[7]s AND bucket = b;
    END IF;
    IF %[9]s OR (d = 0 AND %[10]s) THEN
        %[11]s
    END IF;
    SELECT %[2]s INTO NEW.%[2]s FROM %[12]s WHERE %[13]s;
    RETURN NEW;
END`, s.Type, Q, keySame, quoteLit(s.Table), B, perSet("NEW"), match("", "OLD."), s.N, coldChanged, perChanged, baseUpdate, V, oldKeyMatch, T, s.N-1))
	fn(s.View+"_del", fmt.Sprintf("BEGIN\n    DELETE FROM %s WHERE %s;\n    RETURN OLD;\nEND", T, oldKeyMatch))
	trig := func(name, when, on, fn string) {
		w("CREATE OR REPLACE TRIGGER %s %s ON %s FOR EACH %s EXECUTE FUNCTION %s();\n", q(name), when, on, map[bool]string{true: "STATEMENT", false: "ROW"}[strings.Contains(when, "TRUNCATE")], q(fn))
	}
	trig(s.BucketTable+"_ins", "BEFORE INSERT", T, s.BucketTable+"_set")
	trig(s.BucketTable+"_set", "BEFORE UPDATE OF "+Q, T, s.BucketTable+"_set")
	trig(s.BucketTable+"_rekey", "BEFORE UPDATE OF "+list(s.Key, q), T, s.BucketTable+"_rekey")
	trig(s.BucketTable+"_del", "AFTER DELETE", T, s.BucketTable+"_del")
	trig(s.BucketTable+"_trunc", "AFTER TRUNCATE", T, s.BucketTable+"_trunc")
	trig(s.View+"_upd", "INSTEAD OF UPDATE", V, s.View+"_upd")
	trig(s.View+"_del", "INSTEAD OF DELETE", V, s.View+"_del")
	// 迁移的 Down(factory state-migration): 触发器删掉之后、桶表删掉之前,把合计写回表本身
	restore := []string{fmt.Sprintf("%s = t.%s + coalesce((SELECT sum(b.%s) FROM %s AS b WHERE %s), 0)", Q, Q, Q, B, match("b.", "t."))}
	for _, c := range s.PerBucket {
		restore = append(restore, fmt.Sprintf("%s = greatest(t.%s, (SELECT max(b.%s) FROM %s AS b WHERE %s))", q(c), q(c), q(c), B, match("b.", "t.")))
	}
	w("-- factory:down-restore UPDATE %s AS t SET %s;\n", T, strings.Join(restore, ", "))
	// 已有的行分到桶里(幂等: 只动还没有桶的行)
	w("-- 已有的行分到桶里(幂等: 还没有桶的行)\nUPDATE %s AS t SET %s = t.%s WHERE NOT EXISTS (SELECT 1 FROM %s AS b WHERE %s);\n", T, Q, Q, B, match("b.", "t."))
	return b.String()
}

// list2: ", a, b"(前面带逗号;空 = "")。
func list2(xs []string, f func(string) string) string {
	var out strings.Builder
	for _, x := range xs {
		out.WriteString(", " + f(x))
	}
	return out.String()
}
