# gauss2sql-go

> 作者：raysuen

> 离线解析 openGauss（含 openGauss 衍生库）数据目录堆文件并导出为 SQL / CSV / DDL / 元数据 JSON 的 Go 实现。

gauss2sql-go 是 gauss2sql（Python 版）的 **Golang 完整重写版**，功能完全等同：直接读取 openGauss 数据目录中的堆文件（heap file），不依赖任何在线服务，实现数据库对象的导出与数据还原。

- **零第三方依赖**：纯 Go 标准库实现，`go build` 产出单静态二进制（约 3.2MB）
- **输出与 Python 版逐字节一致**：DDL / SQL / CSV / meta.json / --parallel 输出全部一致
- **全版本兼容**：openGauss 5.0.0 ~ 6.0.6 共 12 个正式发布版本（x86_64）全功能回归 ALL PASS；openGauss 衍生库 MogDB 5.0.9 实测 18/18 ALL PASS
- **全类型支持**：int/float/numeric/text/varchar/char/bool/date/time/timestamp/uuid/inet/bit/varbit/money/bytea/jsonb/xid/tid/枚举/数组（含 NULL 元素）/中文列名

---

## 一、功能特性

- `--list-db`：列出数据目录下全部数据库（OID → 库名映射）
- `--list-tables-db`：列出指定数据库的全部表
- `--export-meta`：导出元数据 JSON（全部表结构，供 --catalog-json 使用）
- `--ddl --sql`：导出表结构 DDL + INSERT 数据 SQL
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
