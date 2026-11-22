# 备忘录: 生成的项目的建表 / 迁移(GAPS G24)

2026-11-23(R4.1)· 状态: **建议,待决定**

## 现状

- 生成的项目只有 `db/schema/*.sql`: 设计者写的表(`CREATE TABLE IF NOT EXISTS …`)+ 工厂写的平台表
  `zz_factory_platform.sql`(运行时的 `outbox.PlatformSQL` 原样复制;文件名排在最后)。没有版本,没有"从 v1 到 v2"的路径。
- 服务器启动时不建表(运行时从不执行 DDL),boundaries §8.1 说建表由平台在部署前执行。e2e 用每个数据库一个一次性的
  `postgres:16-alpine` 容器按文件名顺序 `psql` 执行。
- 工厂仓库自己用 goose(`db/migrations/000NN_*.sql`)+ `Dockerfile.schema`(迁移镜像,Deployment 的 init container)。

## 问题

1. **加一列、加一个索引没有路径**: `CREATE TABLE IF NOT EXISTS` 对已有的表什么都不做。现在 storefront 的数据库都是空的,
   问题还没出现;第一个上线的服务第一次改表就会撞上。
2. **执行的契约没有写下来**: 谁执行、按什么顺序、用什么权限、失败了怎么办,只存在于 e2e 的 compose 里。

## 选项

| | 做法 | 好处 | 代价 |
|---|---|---|---|
| A | 平台层的契约: "部署前按文件名顺序执行 `db/schema/*.sql`,每个文件必须幂等";改表写成新的幂等文件(`ALTER TABLE … ADD COLUMN IF NOT EXISTS`) | 什么都不用加;和现在的文件一致 | 幂等靠人写(`DROP` / 改类型很难写成幂等);没有"执行过哪些"的记录 |
| B | 打包模板也生成迁移镜像: goose + `db/migrations/`(第一个迁移 = 现在的 `db/schema` 的内容),和工厂仓库一样 | 版本化、有记录、和工厂自己的做法一致;平台只要"先跑迁移镜像" | 设计者要同时维护 `db/schema`(差分测试 / 设计的输入)和迁移;要一个检查保证"迁移全部执行后的结构 = db/schema"(工厂仓库的 `scripts/ci/migrations-check.sh` 就是这个) |
| C | 工厂从 `db/schema` 的变化自动生成迁移(比较前后的结构) | 设计者只写最终的结构 | 自动比较结构的工具(改名 vs 删除 + 新建分不出来)是一个独立的大题目,验证也难;不符合 §8 的"能证明" |

## 建议

**现在 A(写下契约),第一个需要改已有表的项目出现时做 B。** 不做 C。

- A 立刻能做,只是文档: 平台在部署前以表的拥有者执行 `db/schema/*.sql`(文件名顺序,`ON_ERROR_STOP`,一个文件一个事务);
  文件必须幂等;`zz_factory_platform.sql` 由工厂维护,永远排最后。e2e 的 schema 容器就是这个契约的参考实现。
- B 是重复的("每个项目都会出现")、能声明的(迁移目录就是数据)、能证明的(迁移执行后的结构 = `db/schema`,
  工厂仓库已经有这个检查),符合 boundaries §8 的三个条件,所以最终应该进工厂的打包模板。但现在没有一个项目
  需要改已有的表,先做会是没有需求来源的功能(readiness §6 的风险 3)。
- 触发条件: storefront v2 第一次在有数据的环境里改表,或者任何项目的 `db/schema` 的 diff 里出现对已有表的修改。
