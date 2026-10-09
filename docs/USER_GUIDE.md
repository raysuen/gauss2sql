# gauss2sql 用户手册

> gauss2sql-go v0.2.12（Author: raysuen）
> openGauss 离线导出工具：直接读取数据目录堆文件，导出 SQL / CSV / DDL / 元数据 JSON

---

## 目录

1. [工具简介](#1-工具简介)
2. [安装与构建](#2-安装与构建)
3. [快速开始](#3-快速开始)
4. [参数详解](#4-参数详解)
5. [参数搭配与场景示例](#5-参数搭配与场景示例)
6. [输出与文件命名规则](#6-输出与文件命名规则)
7. [支持的磁盘格式](#7-支持的磁盘格式)
8. [坏块（损坏数据）处理](#8-坏块损坏数据处理)
9. [导出-导入最佳实践](#9-导出-导入最佳实践)
10. [常见问题 FAQ](#10-常见问题-faq)

---

## 1. 工具简介

**gauss2sql** 是一个纯离线 openGauss 数据导出工具（Golang 实现，单静态二进制，零外部依赖）：

- **不依赖数据库服务**：直接解析 openGauss 数据目录中的堆文件（heap file），数据库可处于停机/只读/归档状态
- **全版本兼容**：openGauss 5.0.0 ~ 6.0.6 全部 12 个正式发布版本（x86_64）全功能回归 ALL PASS；openGauss 衍生库 MogDB 5.0.9 实测 18/18 ALL PASS
- **全类型支持**：int/float/numeric/text/varchar/char/bool/date/time/timestamp/uuid/inet/bit/varbit/money/bytea/jsonb/xid/tid/枚举/数组/中文列名，及 range 全系/tsvector/tsquery/几何/txid_snapshot/reg* 系列/xml
- **输出可导入**：SQL 用 gsql 导入，CSV 用 COPY 导入，逐值还原
- **健壮**：坏块三级处理，不 panic、不丢行（见第 8 节）

**典型使用场景**：
- 数据库异常无法启动时抢救数据
- 停机窗口的快速逻辑备份（无需安装客户端工具）
- 迁移：openGauss → openGauss / 衍生库
- 取证/审计：直接分析数据文件内容

---

## 2. 安装与构建

### 2.1 使用发行包（推荐）

```bash
# 解压执行包（linux amd64 / arm64）
tar xzf gauss2sql-v0.2.12-linux-amd64.tar.gz
./gauss2sql --version     # gauss2sql-go 0.2.12 (Author: raysuen)
```

执行包解压后为单文件 `gauss2sql`，静态链接，目标 Linux 平台直接运行，无任何依赖。

### 2.2 源码构建

环境要求：Go ≥ 1.23。

```bash
# 本机构建
go build -o gauss2sql .

# linux x86_64 交叉编译
GOOS=linux GOARCH=amd64 go build -o gauss2sql-linux-amd64 .

# linux aarch64（ARM64）交叉编译
GOOS=linux GOARCH=arm64 go build -o gauss2sql-linux-arm64 .
```

> Go 交叉编译无需目标平台环境，默认静态链接。

---

## 3. 快速开始

以下命令覆盖 90% 的日常需求（位置参数含义见 4.1）：

```bash
# 1. 单表导出：SQL（DDL + INSERT）
./gauss2sql base/16388/16414 --ddl --sql

# 2. 单表导出：CSV（带表头，输出到指定目录）
./gauss2sql base/16388/16414 --data --header -o /tmp/out/

# 3. 按表名导出（无需知道 relfilenode，位置参数为数据库目录）
./gauss2sql base/16388 --table-name app.users --sql --ddl -o /tmp/gauss2sql/

# 4. 批量导出整个 schema 的所有普通表
./gauss2sql base/16388 --tables --schema app --sql -o /tmp/out/

# 5. 列出数据库中的用户对象
./gauss2sql base/16388 --list-tables-db

# 6. 只统计行数
./gauss2sql base/16388 --count --table-name app.users
```

---

## 4. 参数详解

### 4.1 位置参数 `<data-file-or-db-dir>`

openGauss 堆数据文件路径或数据库目录。

| 形态 | 示例 | 用途 |
|---|---|---|
| 单个数据文件 | `base/16388/16414` | 单表导出（自动发现表结构） |
| 数据库目录 | `base/16388` | 配合 `--table-name` / `--list-tables-db` / `--list-tables-all` / `--export-meta` / `--tables` / `--all-tables` / `--schema` |
| 数据根目录 | `/data/openGauss` | 配合 `--datadir` + `--list-db` |

**路径结构说明**（openGauss 数据目录）：
- `base/<数据库OID>/`：每个数据库一个目录，内含系统目录文件与用户表文件
- 文件名 = relfilenode，普通表/索引/序列/TOAST 各有自己的文件
- `pg_database` 在 `global/` 下，`--list-db` 通过它列出全部数据库

### 4.2 通用选项

| 参数 | 说明 |
|---|---|
| `-h, --help` | 显示帮助信息并退出 |
| `--version` | 显示版本号并退出 |
| `--datadir DIR` | openGauss 数据目录（配合 `--list-db` 使用，如 `/data/openGauss`） |
| `--verbose` | 打印导出过程信息（表结构/TOAST/输出路径/行数/耗时，输出到 **stderr**，不影响数据输出） |

**--verbose 示例**：
```bash
./gauss2sql base/16388/16414 --ddl --sql --verbose
# [verbose] 自动发现表结构: app.users (5 列, relfilenode=16414)
# [verbose] TOAST: base/16388/16412 (light 索引)
# [verbose] 模式: ddl+sql (parallel=1, encoding=utf-8)
# [verbose] 输出到: 标准输出
# [verbose] 完成: 1000 行, 耗时 0.03s
```

### 4.3 元数据相关参数

| 参数 | 说明 |
|---|---|
| `--catalog-json FILE` | 从 JSON 文件加载表结构元数据（由 `--export-meta` 导出）。用于：无系统目录可读、或需要按 JSON 精确控制导出的场景 |
| `--table-name NAME` | 指定表名，**两种用法**：<br>① 配合 `--catalog-json`：从 JSON 中按名匹配表<br>② 位置参数为**数据库目录**时：直查系统目录按名定位表并导出（v0.2.12 起，无需 --catalog-json） |

**--table-name 支持的表名格式**：
- `schema.table`（推荐，精确匹配）：`app.users`、`public.orders`
- 裸表名 `table`：全库唯一时可用；若多个 schema 存在同名表会报错并列出候选，提示用 schema.table

**--table-name 与 --catalog-json 组合示例**：
```bash
# 先导出全库元数据
./gauss2sql base/16388 --export-meta -o /tmp/meta.json

# 再按表名从 JSON 导出（含分区子分区表）
./gauss2sql base/16388/16395 --catalog-json /tmp/meta.json --table-name app.users --ddl --sql
```

### 4.4 输出模式参数（互斥组）

| 参数 | 输出内容 | 典型后缀 |
|---|---|---|
| `--sql` | INSERT 语句（数据尾部自动追加 setval 序列同步） | `.sql` |
| `--ddl` | CREATE TABLE DDL（含列默认值/主键/唯一/CHECK 约束/表列注释/CREATE SEQUENCE 前置/非主键索引） | `.ddl` 或与 --sql 合并 |
| `--data` | CSV 数据（可用 COPY 导入） | `.csv` |
| `--count` | 仅统计行数（不输出数据） | — |
| `--deleted` | 输出已删除**和**未删除的行（t_xmax 已设置但未被 vacuum 清理） | — |
| `--only-deleted` | 只输出已删除的行 | — |

**组合规则**：
- `--ddl --sql`：DDL 在前，INSERT 在后，可直接导入重建
- 纯 `--ddl`：只输出表结构
- `--deleted` / `--only-deleted` 可与 `--sql` / `--data` / `--count` 组合

**示例**：
```bash
# 只导 DDL
./gauss2sql base/16388/16414 --ddl

# 只导数据（不含 DDL）
./gauss2sql base/16388/16414 --sql

# 导出已删除行（用于数据恢复）
./gauss2sql base/16388/16414 --only-deleted --sql
```

### 4.5 列表/枚举参数

| 参数 | 说明 |
|---|---|
| `--list-db` | 列出数据目录中的全部数据库（OID + 名称 + 目录路径）。需 `--datadir` 或位置参数 |
| `--list-tables-db` | 列出指定数据库目录的**用户对象**（普通表/索引/序列/TOAST/视图，默认过滤系统 schema） |
| `--list-tables-all` | 列出全部对象（**含系统 schema**） |
| `--export-meta` | 导出全部表结构为元数据 JSON（供 `--catalog-json` 使用） |

**示例**：
```bash
./gauss2sql --datadir /data/openGauss --list-db
./gauss2sql base/16388 --list-tables-db
./gauss2sql base/16388 --list-tables-all
./gauss2sql base/16388 --export-meta -o /tmp/meta.json
```

### 4.6 批量导出参数（对齐 pg2sql）

| 参数 | 说明 |
|---|---|
| `--tables` | 批量导出全部**用户普通表** |
| `--all-tables` | 批量导出全部普通表（**含系统 schema**） |
| `--schema NAME` | 按 schema 过滤批量导出（逗号分隔多值，等价 `--tables --schema`） |

**批量导出约束**：
- 位置参数必须是**数据库目录**
- 必须指定 `-o` 输出**目录**
- 必须指定 `--sql` / `--data` / `--ddl` 之一
- 每表生成一个 `<schema>.<对象名>.<sql|csv|ddl>` 文件

**示例**：
```bash
# 导出 app schema 全部普通表（每表一个 SQL 文件）
./gauss2sql base/16388 --tables --schema app --sql -o /tmp/out/

# 导出全部用户普通表为 CSV
./gauss2sql base/16388 --tables --data --header -o /tmp/out/

# 导出全库（含系统 schema）DDL
./gauss2sql base/16388 --all-tables --ddl -o /tmp/ddl/
```

### 4.7 输出选项

| 参数 | 说明 |
|---|---|
| `-o, --output PATH` | 输出到文件或目录（规则见第 6 节）；未指定时输出到标准输出 |
| `--parallel N` | 并行导出线程数（默认 1）。输出与串行**逐字节一致** |
| `--limit N` | 只导出前 N 行 |
| `--delimiter STR` | CSV 字段分隔符（默认 `,`，与 COPY DELIMITER 语义一致） |
| `--fields COL1,COL2` | 只导出指定字段（逗号分隔；不存在的字段会报错） |
| `--header` | CSV 首行输出字段名（配合 `--data`，与 COPY HEADER true 兼容） |
| `--encoding ENC` | 数据解码编码。默认自动探测，探测失败或需强制时可指定：UTF8/GBK/GB18030/LATIN1/SQL_ASCII 等，**输出统一为 UTF-8** |

**示例**：
```bash
# 只导两列，CSV 自定义分隔符
./gauss2sql base/16388/16414 --data --fields id,name --delimiter '|' --header -o /tmp/users.csv

# 只导出前 100 行
./gauss2sql base/16388/16414 --sql --limit 100

# GBK 库强制按 GBK 解码
./gauss2sql base/16388/16414 --sql --encoding GBK

# 4 线程并行
./gauss2sql base/16388/16414 --sql --parallel 4
```

---

## 5. 参数搭配与场景示例

### 5.1 场景：单表完整导出（DDL + 数据）

```bash
./gauss2sql base/16388/16414 --ddl --sql -o /tmp/
# 生成 /tmp/app.users.sql（含 CREATE TABLE、约束、INSERT、setval）
```

### 5.2 场景：按表名导出（不知道 relfilenode）

```bash
# 位置参数为数据库目录，--table-name 指定表
./gauss2sql base/16388 --table-name app.users --sql --ddl -o /tmp/gauss2sql/
# 生成 /tmp/gauss2sql/app.users.sql

# 裸表名（全库唯一时）
./gauss2sql base/16388 --table-name users --data --header -o /tmp/

# 分区父表：自动展开全部子分区
./gauss2sql base/16388 --table-name app.sales --sql --ddl -o /tmp/
# 父表 DDL 一次 + 所有分区数据拼接，与逐子分区导出结果逐字节一致
```

### 5.3 场景：批量导出整个 schema

```bash
./gauss2sql base/16388 --tables --schema app,public --sql -o /tmp/out/
# /tmp/out/app.users.sql, /tmp/out/app.orders.sql, ...
```

### 5.4 场景：只导 CSV 并导入目标库

```bash
# 导出
./gauss2sql base/16388/16414 --data --header -o /tmp/users.csv

# 导入（gsql COPY，逐值还原）
gsql -d targetdb -c "COPY app.users FROM '/tmp/users.csv' WITH (FORMAT csv, HEADER true);"
```

### 5.5 场景：导出已删除行（数据恢复）

```bash
./gauss2sql base/16388/16414 --deleted --count     # 统计含已删除行的总数
./gauss2sql base/16388/16414 --only-deleted --sql  # 只导出已删除行
```

### 5.6 场景：中文/特殊字符库（编码处理）

```bash
# 自动探测（UTF8/GBK/GB18030/LATIN1）
./gauss2sql base/16388/16414 --sql

# 强制指定（探测失败时）
./gauss2sql base/16388/16414 --sql --encoding GBK
```

### 5.7 场景：元数据 JSON 闭环（catalog-json）

```bash
# 1. 导出元数据
./gauss2sql base/16388 --export-meta -o /tmp/meta.json

# 2. 用 JSON 按表名导出（适合：系统目录不可读/需精确控制）
./gauss2sql base/16388/16395 --catalog-json /tmp/meta.json --table-name app.users --ddl --sql
```

### 5.8 场景：并行加速大表

```bash
# 4 线程并行，输出与串行逐字节一致
./gauss2sql base/16388/16414 --sql --parallel 4 -o /tmp/big.sql
```

### 5.9 场景：数据库不可启动时抢救数据

```bash
# 直接读数据目录（无需数据库服务）
./gauss2sql /data/openGauss/base/16388 --list-tables-db
./gauss2sql /data/openGauss/base/16388 --tables --sql -o /tmp/rescue/
```

---

## 6. 输出与文件命名规则

`-o/--output` 的取值决定输出位置与文件名（与 Python 版行为一致）：

| -o 取值 | 行为 |
|---|---|
| 未指定 | 输出到**标准输出** |
| 目录（以 `/` 结尾或已存在） | 生成 `<路径>/<schema>.<对象名>.<ext>` |
| 带扩展名的路径 | 原样使用（如 `-o /tmp/users.sql`） |
| 不带扩展名的路径 | 自动追加 `.ext`（如 `-o /tmp/users` → `/tmp/users.sql`） |

**扩展名规则**：`--sql` → `.sql`；`--data` → `.csv`；纯 `--ddl`（不带 --sql）→ `.ddl`。

**批量导出模式**：`-o` 必须是已存在的目录，每表一个文件 `<schema>.<对象名>.<ext>`。

**示例**：
```bash
./gauss2sql base/16388/16414 --sql -o /tmp/            # /tmp/app.users.sql
./gauss2sql base/16388/16414 --data -o /tmp/users.csv  # /tmp/users.csv（原样）
./gauss2sql base/16388/16414 --sql                     # 标准输出
```

---

## 7. 支持的磁盘格式

| 格式项 | 说明 |
|---|---|
| 页布局 | 8KB、version 6、页头 24B |
| HeapTuple | t_xmin/t_xmax 8B xid、t_cid、t_hoff；系统目录行 OID 在 t_hoff-4 |
| 系统目录三级定位 | 标准文件名 → relmapper → pg_class 取 relfilenode（pg_enum/pg_namespace 不在 relmapper） |
| varlena | 4B/1B 头；TOAST Light 内联 [4B头][4B rawsize][PGLZ流]；PGLZ 解压 |
| 外联 TOAST | 18B 指针 [rawsize][extsize][valueid][toastrelid]，toastrelid 经 pg_class 映射物理文件 |
| jsonb JEntry | 键值交错、低 28 位绝对终点、类型位、container 对齐 |
| tid | [hi 2B][lo 2B][pos 2B] 全小端，blk=(hi<<16)\|lo |
| 分区表 | 父表 relfilenode=0，子分区在 pg_partition，回查 parentid → pg_class 父表 → 继承列定义 |

---

## 8. 坏块（损坏数据处理）

坏块三级处理，尽力恢复，全程不 panic：

| 坏块级别 | 检测方式 | 实际动作 | 效果 |
|---|---|---|---|
| **页头损坏**（整页不可信） | 页头 pd_lower/pd_upper 非法 | 标准 ItemId 遍历跳过该页并记录，随后对该页做**数据区扫描补漏**（绕过不可信页头/ItemId 直接找 tuple） | 坏页行恢复导出；实测破坏任意页页头 20000 行不 panic、**不丢行** |
| **单个坏行**（tuple 头损坏） | tuple 头解析失败 | 仅丢弃该行，继续后续行 | 其余行正常导出，不扩散 |
| **字段级损坏**（varlena 越界 / 位串 / 数组位图 / range 越界） | 解码边界校验 | varlena 越界该行截断；位串/数组/range 越界该列输出空值或降级文本 | 行保留或单行丢弃，**全程不崩溃** |

- 损坏 meta.json / CLI 缺参：安全报错退出
- 坏块只影响该页/该行，不影响整表其余数据正确性（正常数据与旧版逐字节一致，无回归）

---

## 9. 导出-导入最佳实践

### 9.1 导出侧建议

1. **先勘察**：`--list-db` → `--list-tables-db` 确认对象
2. **小表试跑**：先导 1-2 张表核对内容（`--verbose` 查看过程）
3. **正式导出**：批量用 `--tables --schema X --sql -o dir`；大表加 `--parallel 4`
4. **校验**：`--count` 对比源库与目标库行数；抽样对比特殊字符/中文列

### 9.2 导入侧建议（SQL 方式）

```bash
gsql -d targetdb -f /tmp/app.users.sql
```

- 文件内含 `CREATE TABLE`、约束、INSERT、`setval` 序列同步，可直接导入
- 若仅迁移数据（目标已有表结构）：用 `--sql`（不带 `--ddl`）导出

### 9.3 导入侧建议（CSV 方式）

```bash
gsql -d targetdb -c "COPY app.users FROM '/tmp/users.csv' WITH (FORMAT csv, HEADER true);"
```

- 表结构需先建好（用 `--ddl` 或手工建表）
- 自定义分隔符：导出 `--delimiter '|'`，导入 `WITH (FORMAT csv, DELIMITER '|', HEADER true)`

### 9.4 特殊数据类型注意事项

| 类型 | 说明 |
|---|---|
| 数组 | 字面量 `"NULL"` 输出带引号 `{"NULL",...}`；SQL NULL 元素输出裸 `NULL` |
| timetz | 时区符号与 PostgreSQL 相反（openGauss 磁盘约定），导出与 `::text` 逐值一致 |
| bytea/jsonb | 大对象自动走 TOAST（内联/外联均支持），无截断 |
| 枚举 | DDL 前置 `CREATE TYPE ... AS ENUM`，数据用枚举成员名 |

---

## 10. 常见问题 FAQ

**Q1：位置参数给了目录却报错？**
目录形态只支持 `--table-name` / `--list-tables-*` / `--export-meta` / `--tables` / `--all-tables` / `--schema`。其他模式（`--sql`/`--data`/`--ddl` 不带表名）需要**具体数据文件**路径。

**Q2：`--table-name` 裸表名报"多个 schema 下存在"？**
用 `schema.table` 精确指定，如 `app.users`。

**Q3：CSV 导出的中文乱码？**
自动探测失败时用 `--encoding GBK`（或 GB18030/UTF8）强制指定；输出统一为 UTF-8。

**Q4：批量导出提示必须指定 -o？**
批量模式（`--tables`/`--all-tables`/`--schema`）必须 `-o` 已存在目录 + `--sql`/`--data`/`--ddl` 之一。

**Q5：--parallel 结果和串行不一致？**
不应发生（设计保证逐字节一致）。如出现，检查是否同一版本二进制。

**Q6：导入时自增列冲突？**
SQL 尾部含 `setval` 序列同步，直接导入即可；若只导数据（`--sql` 不带 `--ddl`），同样含 setval。

**Q7：支持 openGauss 7.x 吗？**
7.0.0-RC 系列未纳入正式回归矩阵；磁盘格式无变化时可尝试（工具按 pg_class 实时定位，对版本差异免疫）。正式发版前建议先行验证。

**Q8：能在 Windows 上用吗？**
发行包为 Linux amd64/arm64。Windows 可用 WSL 运行，或源码交叉编译（GOOS=windows 需 CGO 关闭，Go 标准库即可）。

---

*本手册与 gauss2sql-go 源码同版本维护；参数行为以 `gauss2sql --help` 与源码为准。*
