# gauss2sql-go

> 作者：raysuen

> 当前版本：**v0.2.5**（变更记录见[第五节](#五变更记录)）

> 离线解析 openGauss（含 openGauss 衍生库）数据目录堆文件并导出为 SQL / CSV / DDL / 元数据 JSON 的 Go 实现。

gauss2sql-go 是 gauss2sql（Python 版）的 **Golang 完整重写版**，功能完全等同：直接读取 openGauss 数据目录中的堆文件（heap file），不依赖任何在线服务，实现数据库对象的导出与数据还原。

- **零第三方依赖**：纯 Go 标准库实现（v0.1.7 起引入 `golang.org/x/text` 支持 GBK/GB18030 转码），`go build` 产出单静态二进制（约 3.2MB）
- **输出与 Python 版逐字节一致**：DDL / SQL / CSV / meta.json / --parallel 输出全部一致
- **全版本兼容**：openGauss 5.0.0 ~ 6.0.6 共 12 个正式发布版本（x86_64）全功能回归 ALL PASS；openGauss 衍生库 MogDB 5.0.9 实测 18/18 ALL PASS
- **全类型支持**：int/float/numeric/text/varchar/char/bool/date/time/timestamp/uuid/inet/bit/varbit/money/bytea/jsonb/xid/tid/枚举/数组（含 NULL 元素）/中文列名

---

## 一、功能特性

- `--list-db`：列出数据目录下全部数据库（OID → 库名映射）
- `--list-tables-db`：列出指定数据库的全部表
- `--export-meta`：导出元数据 JSON（全部表结构，供 --catalog-json 使用）
- `--ddl --sql`：导出表结构 DDL + INSERT 数据 SQL（DDL 含列 DEFAULT、表级 PRIMARY KEY / UNIQUE / CHECK 约束、表/列 COMMENT，v0.2.3 起）
- `--data --output`：导出 CSV 数据文件
- `--parallel N`：并行导出，输出与串行逐字节一致
- `--deleted / --only-deleted`：导出（仅导出）已删除行（支持 ctid 定位）
- `--count`：仅统计行数
- `--fields / --header / --encoding`：字段过滤 / 表头 / 字符集解码编码（UTF8/GBK/GB18030/LATIN1 等）
- `--catalog-json --table-name`：配合元数据 JSON 按表名导出（含分区子分区表）

### 支持的 openGauss 磁盘格式（已破解并移植）

| 格式项 | 说明 |
|---|---|
| 页布局 | 8KB、version 6、页头 24B |
| HeapTuple | t_xmin/t_xmax 8B xid、t_cid、t_hoff；系统目录行 OID 在 t_hoff-4 |
| 系统目录三级定位 | 标准文件名 → relmapper → pg_class 取 relfilenode（pg_enum/pg_namespace 不在 relmapper） |
| varlena | 4B/1B 头；TOAST Light 内联 [4B头][4B rawsize][PGLZ流]（压缩判定低 2 位==2）；PGLZ 解压 |
| 外联 TOAST | 18B 指针 [rawsize][extsize][valueid][toastrelid]，toastrelid 经 pg_class 映射物理文件 |
| jsonb JEntry | 对象键值交错、低 28 位=绝对终点偏移、类型位（container/false/null/true 等）、container/numeric 起点 INTALIGN4 对齐 |
| tid | [hi 2B][lo 2B][pos 2B] 全小端，blk=(hi<<16)\|lo |
| 分区表 | 父表 relfilenode=0，子分区在 pg_partition，回查 parentid → pg_class 父表 → 继承列定义 |

---

## 二、构建

环境要求：Go ≥ 1.23。

```bash
# 本机构建（linux amd64）
go build -o gauss2sql-go .

# linux x86_64 交叉编译
GOOS=linux GOARCH=amd64 go build -o gauss2sql-go-linux-amd64 .

# linux aarch64（ARM64）交叉编译
GOOS=linux GOARCH=arm64 go build -o gauss2sql-go-linux-arm64 .
```

> Go 交叉编译无需目标平台环境，`CGO_ENABLED=0` 默认静态链接，产物可在对应 Linux 平台直接运行。

---

## 三、用法

```
gauss2sql-go <数据文件或数据目录> [options]
```

常用示例：

```bash
# 查看全部数据库
gauss2sql-go --datadir /data/openGauss --list-db

# 列出某库的全部表
gauss2sql-go --datadir /data/openGauss --list-tables-db --db-oid 16388

# 导出元数据 JSON
gauss2sql-go --datadir /data/openGauss --export-meta -o meta.json

# 导出某表 DDL + SQL
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --ddl --sql

# 导出 CSV (指定文件)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --data --output t_all.csv

# 导出 CSV (目录自动命名: /tmp/app.t_all_types.csv)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --data --header -o /tmp/

# 未指定输出: 结果输出到标准输出 (可重定向)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --sql > app.t_all_types.sql

# 并行导出（4 线程）
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_big --sql --parallel 4

# 已删除行统计 / 导出
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_del --deleted --count
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_del --only-deleted --sql
```

### CLI 参数一览

| 参数 | 说明 |
|---|---|
| `--datadir` | openGauss 数据目录（配合 --list-db 等） |
| `--list-db` | 列出数据库 |
| `--list-tables-db` | 列出指定库的表（需 `--db-oid`） |
| `--export-meta` | 导出元数据 JSON |
| `--ddl` | 输出建表 DDL |
| `--sql` | 输出 INSERT 数据 SQL |
| `--data` | 输出 CSV（需 `--output`） |
| `--output` / `-o` | 输出到文件或目录；**未指定时输出到标准输出**；目录(以`/`结尾或已存在)自动生成 `<路径>/<schema>.<对象名>.<sql|csv>`；不带扩展名的路径自动追加 `.sql/.csv` |
| `--parallel N` | 并行线程数 |
| `--deleted` | 包含已删除行 |
| `--only-deleted` | 仅导出已删除行 |
| `--count` | 仅统计行数 |
| `--fields` | 指定导出字段 |
| `--header` | CSV 输出表头 |
| `--encoding` | 库数据解码编码（自动探测；探测失败时可指定 `UTF8`/`GBK`/`GB18030`/`LATIN1`/`SQL_ASCII` 等，输出统一为 UTF-8） |
| `--verbose` | 打印导出信息到 stderr（表结构/TOAST/输出路径/行数/耗时） |
| `--catalog-json` | 元数据 JSON 路径 |
| `--table-name` | 按表名导出（支持 schema.table） |
| `--version` | 版本信息 |

---

## 四、测试与回归

自动化回归框架：`run_regression_go.sh`（配合 `test_data.sql` 造数脚本）。

| 测试线 | 结果 | 报告 |
|---|---|---|
| openGauss 全版本（5.0.0~6.0.6，12 个正式版本） | 12/12 ALL PASS | `REGRESSION-GO.md` |
| Python 版输出一致性（5.0.5 基准逐字节 diff） | 零差异 | `REGRESSION-GO.md` |
| 50 字段大批量（50,052 行，中英文混搭 + 特殊字符全集） | ALL PASS | `REGRESSION-GO.md` |
| openGauss 衍生库 MogDB 5.0.9 | 18/18 ALL PASS | `REGRESSION-DERIVED.md` |

---

## 五、变更记录

- **v0.2.5（2026-09-30）**：文档与帮助文本同步轮——README 头部当前版本标注更新为 **v0.2.5**；功能特性 `--ddl` 描述补充 DDL 增强说明（默认值/主键/唯一/CHECK/注释）；`--help` 的 `--ddl` 说明同步更新为"输出 CREATE TABLE DDL（含默认值/主键/唯一/CHECK/表列注释）"。无逻辑变更，仅文档与帮助文本。
- **v0.2.4（2026-09-29）**：**修复 DDL 增强（P1-④）3 处源码 bug**——v0.2.3 的 `--ddl` 增强在真实 openGauss 集群上实测完全不生效，本轮修复后 8 项增强元素齐全且无乱码：①`pgAttrdefOID` 由错误的 `2600`（实为 pg_aggregate）改正为 **2604**（pg_attrdef 全版本 OID），列 `DEFAULT`（含 `nextval(...)`/`'unknown'`/`0`）不再丢失；②`pg_constraint` 列布局重排——删除 openGauss 不存在的幻影列 `conparentid`，补齐 6.0.x 新增的 `conislocal/coninhcount/connoinherit/consoft/conopt` 5 列，修正后 `conkey=18`、`consrc=25`（此前偏移错位致 PK/UNIQUE/CHECK 全丢），同时 `decodeInt2Array` 改走 `VarlenaParse` 剥头并跳过数组维头（nelems+lowerbound 8B），主键列不再被重复展开成 `("id","id","id")`；③`LoadDescriptions`/`attrdefFields`/`constraintFields` 的 varlena 文本字段统一经 `varlenaText()` 剥 1B/4B varlena 头取负载，表/列注释不再带前导脏字节（此前表注释 `'DDL增强测试表`、列注释 `=状态:...`）。实测：`--ddl` 8/8 元素齐全、导出 SQL 导入新库逐值零差异、catalog-json 与直连逐字节一致、无约束表 t_all_types 与 v0.2.2 逐字节一致（无回归）。**已知边界**：`DEFAULT nextval(...)` 引用的序列本轮仍不导出 `CREATE SEQUENCE`，导入前需手动 `CREATE SEQUENCE`（或在新库先建序列），否则建表报 `relation "..._seq" does not exist`。
- **v0.2.3（2026-09-29）**：**DDL 增强（P1-④）**——`--ddl` 新增输出：列 `DEFAULT`（解析 pg_attrdef 的 adsrc，含序列 nextval 默认值）、表级约束 `PRIMARY KEY` / `UNIQUE` / `CHECK`（解析 pg_constraint 的 conkey/consrc，跳过外键与未验证约束）、`COMMENT ON TABLE/COLUMN`（解析 pg_description，引号转义）；meta.json 表对象新增 `oid` 字段，`primary_key` 首次真正填充（此前恒空）；`--catalog-json` 模式重建 Reloid 后同样可输出增强 DDL；无约束/默认值/注释的表 DDL 与旧版逐字节一致（无回归）；export-meta 预分组约束避免每表全扫 pg_constraint。
- **v0.2.2（2026-09-29）**：README 头部显式注明**当前版本 v0.2.2**（含变更记录锚点）；`--help` 补充说明 `--table-name` 未指定时按 relfilenode 自动匹配；README "零第三方依赖" 措辞更正（v0.1.7 起引入 golang.org/x/text 支持 GBK/GB18030）。无逻辑变更，仅文档与帮助文本。
- **v0.2.1（2026-09-29）**：修复 `--export-meta` **P1 缺陷**——export-meta 分支在 `main.go` 提前 return，走不到直连路径的 `catalog.LoadEnumMap(dbDir)`，导致导出的 `meta.json` 顶层 `enums`/`enum_labels` **恒为空**（5.0.5/6.0.5 皆然）；连锁后果是 `--catalog-json` 加载后枚举表缺 `CREATE TYPE`、枚举值解码为空串 `''`、导出无法导入新库（`type "mood" does not exist`）。修复：在 `ExportMetaToJSON` 开头补一次 `LoadEnumMap(dbDir)`（与直连路径一致），现 `meta.json` 正确含全部 `pg_enum` 枚举（如 `app.mood: sad/ok/happy`），catalog-json 导出与直连导出**逐字节一致**（含 `CREATE TYPE "mood" AS ENUM (...)` 与 `'happy'` 值）。
- **v0.2.0（2026-09-29）**：`--catalog-json` **完整闭环**——export-meta 补齐 `toastrelid`/`relkind`/`type_names`（类型 OID→名称全映射）与顶层 `enums`（成员 OID→标签，数据解码用）+ `enum_labels`（有序标签，DDL 重建 `CREATE TYPE` 用）；catalog-json 加载时完整重建这些元数据并注入枚举，**TOAST 大字段/枚举/自定义类型均与直连导出逐字节一致**；未指定 `--table-name` 时自动按 relfilenode 匹配；同时修复 varlena 压缩解码越界 panic（脏布局数据不再崩溃）。此前 catalog-json 缺 type_names/toastrelid/枚举，非内置类型输出 `oid:N`、TOAST 大字段导出为 `__TOAST_MISSING__`。
- **v0.1.9（2026-09-29）**：`--parallel/-j` **真实生效**——页级并行导出（此前为空操作参数）：按页范围分片、多 goroutine 并行解析、段间按页序合并，输出与串行**逐字节一致**；TOAST 整文件读缓存（消除每次 Resolve 重复全读，大批量导出提速）；修复 `roleNameMap` 并发写竞态（race 冒烟 0 数据竞争）。
- **v0.1.8（2026-09-29）**：修复数据正确性缺陷——**空字符串 `''` 与 `NULL` 严格区分**：解码层引入内部 NULL 哨兵（`\x00__NULL__`，与真实数据不冲突），SQL 导出空串为 `''`、NULL 为 `NULL`，CSV 导出空串为空字段、NULL 为 `\N`（COPY 语义）。此前空串被误导出为 NULL，导入后数据失真。
- **v0.1.7（2026-09-29）**：新增非 UTF-8 字符集正确转码导出——引入 Go 官方扩展库 `golang.org/x/text`；修正 openGauss 服务端编码枚举探测（源码 `pg_wchar.h` 确认：GBK=6、UTF8=7、LATIN1=9、GB18030=36，与标准 PostgreSQL 不同）；`GBK/GB18030` 库中文数据导出自动转码为 UTF-8（`--encoding gbk|gb18030` 亦可手动指定），UTF-8 库行为不变，解码失败一律 latin-1 逐字节兜底保证字节可逆。
- **v0.1.6（2026-09-29）**：输出规则调整——不指定 `-o` 时结果输出到标准输出；`-o` 指定为目录时生成 `<路径>/<schema>.<对象名>.<sql|csv>`。
- **v0.1.5（2026-09-29）**：新增 `--verbose`——打印导出信息（表结构/TOAST/输出路径/模式/行数/耗时），输出到 stderr 不污染导出文件。
- **v0.1.4（2026-09-29）**：新增 `-h/--help` 完整命令行帮助（含 `-o` 目录自动命名规则说明），对齐 Python 版帮助结构。
- **v0.1.3（2026-09-29）**：修复 `-o/--output` 输出路径处理——指定为目录时自动生成 `<schema>.<对象名>.<sql|csv>`（如 `-o /tmp/` 生成 `/tmp/app.t_simple.csv`）；未指定 `-o` 时默认当前目录生成同名文件；不带扩展名的路径自动追加 `.sql/.csv`。
- **v0.1.2（2026-09-29）**：发布打包——新增 README.md、MIT LICENSE；交叉编译 linux amd64 / arm64 执行版本。
- **v0.1.1**：修复 `--list-db` 未解析库名缺陷（读 pg_database 做 OID→datname 映射，过滤 pgsql_tmp）；全版本重跑 12/12 ALL PASS。
- **v0.1.0**：Golang 完整重写（移植 gauss2sql Python 版全部 8 模块与磁盘格式解析逻辑）。

---

## 六、许可证

本项目采用 **MIT License**，详见 [LICENSE](LICENSE)。

Copyright (c) 2026 raysuen
