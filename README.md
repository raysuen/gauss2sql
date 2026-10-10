# gauss2sql-go

> 作者：raysuen

> 当前版本：**v0.2.15**（变更记录见[第五节](#五变更记录)）

> 离线解析 openGauss（含 openGauss 衍生库）数据目录堆文件并导出为 SQL / CSV / DDL / 元数据 JSON 的 Go 实现。

gauss2sql-go 是 gauss2sql（Python 版）的 **Golang 完整重写版**，功能完全等同：直接读取 openGauss 数据目录中的堆文件（heap file），不依赖任何在线服务，实现数据库对象的导出与数据还原。

- **零第三方依赖**：纯 Go 标准库实现（v0.1.7 起引入 `golang.org/x/text` 支持 GBK/GB18030 转码），`go build` 产出单静态二进制（约 3.2MB）
- **输出与 Python 版逐字节一致**：DDL / SQL / CSV / meta.json / --parallel 输出全部一致
- **全版本兼容**：openGauss 5.0.0 ~ 6.0.6 共 12 个正式发布版本（x86_64）全功能回归 ALL PASS；openGauss 衍生库 MogDB 5.0.9 实测 18/18 ALL PASS
- **全类型支持**：int/float/numeric/text/varchar/char/bool/date/time/timestamp/uuid/inet/bit/varbit/money/bytea/jsonb/xid/tid/枚举/数组（含 NULL 元素）/中文列名，及 openGauss 可用高级类型 range 全系（int4range/numrange/daterange/tsrange 等）/tsvector/tsquery/几何（point/lseg/path/box/polygon/circle）/txid_snapshot/reg* 系列/xml

---

## 一、功能特性

- `--list-db`：列出数据目录下全部数据库（OID → 库名映射）
- `--list-tables-db`：列出指定数据库目录的用户对象（普通表/索引/序列/TOAST/视图，默认过滤系统 schema，对齐 pg2sql）
- `--list-tables-all`：列出指定数据库目录的全部对象（含系统 schema）
- `--export-meta`：导出元数据 JSON（全部表结构，供 --catalog-json 使用）
- `--ddl --sql`：导出表结构 DDL + INSERT 数据 SQL（DDL 含列 DEFAULT、表级 PRIMARY KEY / UNIQUE / CHECK 约束、表/列 COMMENT、`CREATE SEQUENCE` 前置、非主键唯一索引、数据尾部 setval 序列同步，v0.2.6 起）
- `--data --output`：导出 CSV 数据文件（`--delimiter` 自定义分隔符，`--limit` 限行数）
- `--tables / --all-tables / --schema`：批量导出（对齐 pg2sql）——位置参数为数据库目录，必须 `-o` 目录 + `--sql`/`--data`/`--ddl` 之一，每表一个 `<schema>.<对象名>.<sql|csv|ddl>` 文件
- `--parallel N`：并行导出，输出与串行逐字节一致
- `--deleted / --only-deleted`：导出（仅导出）已删除行（支持 ctid 定位）
- `--count`：仅统计行数
- `--fields / --header / --encoding`：字段过滤 / 表头 / 字符集解码编码（UTF8/GBK/GB18030/LATIN1 等）
- `--catalog-json --table-name`：配合元数据 JSON 按表名导出（含分区子分区表）
- `--table-name` 表名直查导出（v0.2.12 起）：位置参数为**数据库目录**时，直查系统目录（pg_class/pg_namespace/pg_attribute）按名定位表并导出，无需 --catalog-json——支持 `schema.table` 与裸表名（后者要求全库唯一，多 schema 同名报错）、分区父表自动展开全部子分区（子分区逐字节等于各分区文件拼接）

### 大表流式导出与多段文件支持（v0.2.13 起，v0.2.14 内存峰值修复）

- **流式化（A 优化）**：分块读（每块 512 页=4MB 窗口）+ 行产出后流式写盘，替代旧版"整文件 ReadFile + 全量文本拼接"——旧版需 ~3.3GB+（接近本机内存时 OOM/swap 无法导出）；v0.2.14 起串行模式同步顺序循环 + 并行有界窗口派发，**串行峰值实测 26MB、`--parallel 4` 峰值实测 140MB**（180 万行 1.47GB 表完整导出）
- **块级并行解析+转义（B 优化）**：`--parallel N` 下每 worker 独立完成"块解析 → 行转义 → 文本产出"，输出按块序合并，与串行**逐字节一致**；多核下转义（CPU 密集）不再受单线程限制
- **>1GiB 多段文件自动支持**：openGauss 表文件超过 1GiB 自动分段（主文件 + `.1`/`.2`...，页号全局续接）。v0.2.13 起自动识别全部段文件并顺序导出——**修复旧版只读主段导致的大表静默丢数据缺陷**。实测 180 万行 1.47GB 表（主段 1GiB + `.1` 段 382MB）`--count` 精确 = 数据库实际行数

### 内存恒定：流式化的核心收益（v0.2.14 起）

**原理**：gauss2sql 导出内存与表大小无关，只受并行度影响。流式内核以**固定窗口**处理数据——每块固定 512 页（4MB 读窗口），串行模式同步顺序循环（同一时刻仅 1 块在内存，零 goroutine、零积压）；并行模式有界窗口派发（在途未完成 ≤ workers×2 块，pending 积压有界）。块处理完立即写出并释放，**内存峰值 = O(窗口×单块)**，不随表文件大小增长。

**实测对比**（openGauss 6.0.6、180 万行 1.47GB 表、4 核）：

| 版本 | 导出方式 | 峰值内存 | 说明 |
|---|---|---|---|
| v0.2.12（旧版） | 整文件读入 + 全量拼接 | ~4-5× 输出大小（1GB 表需 ~3.3GB+） | 接近本机内存时 OOM/swap，无法导出 |
| v0.2.13 | 流式但并发扇出 | 3.26GB | 全部块 goroutine 同时读入，3GB 环境 3 次 OOM |
| **v0.2.14** | 串行流式 | **28MB（恒定）** | 180 万行完整导出 rc=0 |
| **v0.2.14** | `--parallel 4` 流式 | **140MB（恒定）** | 与串行输出逐字节一致 |

**大表外推**（内存恒定，与表大小无关）：

| 表大小 | 串行内存 | parallel 4 内存 | 说明 |
|---|---|---|---|
| 5 GB | 28MB | 140MB | 恒定 |
| 10 GB | 28MB | 140MB | 恒定 |
| 20 GB | 28MB | 140MB | 恒定 |
| 30 GB | 28MB | 140MB | 恒定 |
| 50 GB | 28MB | 140MB | 恒定 |

**实践意义**：
- 50GB 级大表导出峰值仅需 **~140MB（parallel 4）**，普通 2-4GB 内存服务器即可承载任意规模导出，无需预留输出大小 4-5 倍内存
- `--parallel N` 内存近似线性：**≈35MB × N**（实测 4 worker 140MB），按机器内存余量选择并行度即可
- 唯一随表增长的开销是**磁盘 IO 与耗时**（线性），内存不再是瓶颈

### 坏块（损坏页/损坏数据）健壮性支持（v0.2.8 起）

不是简单跳过坏块，而是按坏块程度**三级处理、尽力恢复**，全程不 panic：

| 坏块级别 | 检测方式 | 实际动作 | 效果 |
|---|---|---|---|
| **页头损坏**（整页不可信） | 页头 pd_lower/pd_upper 非法（`HasValidLayout=false`） | 标准 ItemId 遍历跳过该页并记录，随后对该页做**数据区扫描补漏**（绕过不可信页头/ItemId 直接找 tuple） | 坏页行恢复导出；实测破坏正常表任意页页头 20000 行全量导出不 panic、**不丢行** |
| **单个坏行**（tuple 头损坏） | tuple 头解析失败 | 仅丢弃该行，`continue` 继续后续行 | 其余行正常导出，不扩散 |
| **字段级损坏**（varlena 越界 / 位串 / 数组位图 / range 越界） | 解码边界校验 | varlena 越界该行截断（表现为单行丢弃）；位串/数组/range 越界该列输出空值或降级文本 | 行保留或单行丢弃，**全程不崩溃** |

- 损坏 meta.json / CLI 缺参：安全报错退出
- 坏块只影响该页/该行，不影响整表其余数据正确性（正常数据与旧版逐字节一致，无回归）

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

# 列出某库的用户对象 / 全部对象
gauss2sql-go base/16388 --list-tables-db
gauss2sql-go base/16388 --list-tables-all

# 批量导出用户表（对齐 pg2sql；位置参数为数据库目录，必须 -o 目录 + --sql/--data/--ddl）
gauss2sql-go base/16388 --tables --sql -o /tmp/out
gauss2sql-go base/16388 --tables --schema app,public --sql -o /tmp/out
gauss2sql-go base/16388 --all-tables --ddl -o /tmp/ddl   # 含系统 schema 纯 DDL，后缀 .ddl

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
| `--list-tables-db` | 列出指定库的用户对象（普通表/索引/序列/TOAST/视图，过滤系统 schema） |
| `--list-tables-all` | 列出指定库的全部对象（含系统 schema） |
| `--export-meta` | 导出元数据 JSON |
| `--tables` | 批量导出全部用户普通表（位置参数为数据库目录，需 `-o` 目录 + `--sql`/`--data`/`--ddl`） |
| `--all-tables` | 批量导出全部普通表（含系统 schema） |
| `--schema` | 批量导出按 schema 过滤（逗号分隔多值，等价 `--tables --schema`） |
| `--ddl` | 输出建表 DDL（含 DEFAULT/约束/注释/CREATE SEQUENCE/非主键索引） |
| `--sql` | 输出 INSERT 数据 SQL（数据后追加 setval 序列同步） |
| `--data` | 输出 CSV（需 `--output`；`--delimiter` 自定义分隔符） |
| `--output` / `-o` | 输出到文件或目录；**未指定时输出到标准输出**；目录(以`/`结尾或已存在)自动生成 `<路径>/<schema>.<对象名>.<sql|csv|ddl>`；不带扩展名的路径自动追加 `.sql/.csv` |
| `--parallel N` | 并行线程数 |
| `--limit N` | 只导出前 N 行 |
| `--delimiter STR` | CSV 字段分隔符（默认 `,`） |
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

- **v0.2.15（2026-10-10）**：文档轮——README 功能特性「大表流式导出与多段文件支持」小节新增**「内存恒定：流式化的核心收益（v0.2.14 起）」**详细描述：固定窗口流式原理（串行同步零积压 / 并行有界窗口派发）、实测对比表（v0.2.12 需 ~3.3GB+、v0.2.13 并发扇出 3.26GB、v0.2.14 串行 28MB / parallel 4 140MB）、5-50GB 内存恒定外推表、实践意义（50GB 导出仅需 ~140MB、≈35MB×N 线性规则）。无代码逻辑变更，仅文档；版本号同步 +0.1。

- **v0.2.14（2026-10-10）**：**大表内存峰值修复 + 坏块补漏重复行修复**（v0.2.13 回归发现）——
  - **修复 runChunked 并发扇出 OOM**：v0.2.13 的 `runChunked` 一次性扇出全部块 goroutine（大表 357 块 × 每块 4MB 读缓冲+行文本），`workers` 仅限制输出通道缓冲、不限制并发处理数 → 串行模式亦瞬时并发 ~2.9GB，3GB 环境 3 次 OOM（rc=137，峰值 RSS 3.26GB）。修复：串行模式（workers==1）改为**同步顺序循环**（零 goroutine 零 pending，实测 180 万行 1.47GB 表导出峰值 RSS **26MB**）；并行模式改为**主循环驱动的有界窗口派发**（窗口=workers×2，乱序完成不再导致 pending 无界积压，实测 parallel 4 峰值 RSS **140MB**，旧 sem 方案 1.8GB）。
  - **修复坏块补漏重复行**：流式化后坏页补漏按整块（512 页）扫描数据区，把 Phase1 已正常导出的同块其他页行**重复输出**（实测 50052 行表破坏一页页头后多出 958 行）。修复：补漏仅读坏页 1 页并单页数据区扫描。修复后与 v0.2.12 相同破坏的输出**逐字节一致**（md5 全同、行数 50052 不丢不多）。
  - **大表专项（本机 4GB 实测通过）**：`--count`=1800000（与 DB 一致）、串行全量 `--sql` 导出 180 万行 rc=0（峰值 26MB）、`--parallel 4` 与串行 **md5 逐字节一致**（峰值 140MB）——v0.2.13 因 OOM 未完成的路径全部完成。
  - **回归**：50 字段表 50052 行 12 模式（含 limit5000+parallel4）与 v0.2.12 **md5 全同**；坏块 3 模式内部一致且与 v0.2.12 逐字节一致；全版本回归见 REGRESSION-GO.md v0.2.14 章节。无新 CLI 参数，`--help` 无需变更。

- **v0.2.13（2026-10-09）**：**A+B 大表导出优化 + 多段文件支持（正确性修复）**——
  - **A 流式化**：`streamRowsCore` 分块读（每块 512 页=4MB 窗口）+ 行产出流式写盘，替代"整文件 ReadFile + `[]Row`/`[]string` 全量 + strings.Builder 拼接"；`detectSize` 改为仅读文件头 130 字节（旧版整读整个表文件）；内存峰值从 ~4-5 倍输出大小降至 O(workers×块)（实测 1GB 表导出峰值降至 ~200MB 量级，旧版 ~3.3GB+，接近本机内存时旧版 OOM/swap 无法导出、新版本机完成）。
  - **B 块级并行解析+转义**：`--parallel N` 下每 worker 独立完成块解析→行转义→文本产出，按块序合并输出，与串行**逐字节一致**；转义（CPU 密集）多核并行，4 核表导出提速约 2-3 倍。
  - **>1GiB 多段文件支持（关键正确性修复）**：openGauss 表文件超 1GiB 自动分段（主文件 + `.1`/`.2`...，页号全局续接）。旧版仅读主段 → **大表静默丢数据**（1.47GB 表只导出主段 1GiB 的 1310720 行，丢 .1 段 469280 行）。v0.2.13 `listSegments`/`locateSeg`/`scanTuplesRangeSeg` 自动识别全部段文件、页号全局续接（ctid 正确），`--count` 实测 1800000 行与数据库 count 精确一致。
  - **CLI/输出链重构**：main 输出链改为流式（`openOutWriter`+bufio+`streamTable`/`countTable`，批量/分区父表/表名直查全部接入流式）；`StreamSQL`/`StreamToData`/`StreamRowsCount` 新 API；旧 `DumpRows`/`ToSQL`/`ToData` 保留未删。
  - **回归**：50 字段表 50052 行 12 模式（sql/sql-p4/data/data-h/ddl/ddl+sql/limit100/fields/deleted/data-p4/ddl+p4/only-del）与 v0.2.12 **逐字节一致（md5 全同）**；坏块 3 模式一致；1GB 表串行/`GOMEMLIMIT=800MiB` 导出输出 md5 一致；跨版本全功能回归见 REGRESSION-GO.md v0.2.13 章节。无新 CLI 参数，`--help` 无需变更。
- **v0.2.11（2026-10-08）**：文档轮——README 坏块说明从功能特性列表抽离为**独立小节**，并以**三级处理表格**（页头损坏→数据区扫描补漏 / 单个坏行→丢弃 / 字段级损坏→安全降级 + 检测方式/动作/效果）呈现。无逻辑与代码变更，仅文档。
- **v0.2.10（2026-10-08）**：文档轮——README 坏块说明扩写为**三级处理结构**（页头损坏→数据区扫描补漏 / 单个坏行→丢弃 / 字段级损坏→安全降级），与实测行为逐条对应。无逻辑与代码变更，仅文档。
- **v0.2.9（2026-10-08）**：文档轮——README 功能特性补充**坏块（损坏页/损坏数据）健壮性支持**说明（v0.2.8 的 C2 坏页补漏 / B3-B6 越界保护 / B1-B2 安全退出行为成文描述）。无逻辑与代码变更，仅文档。
- **v0.2.8（2026-10-08）**：**深度审查 20 项缺陷修复轮（数据正确性 A1/A2、可崩溃输入 B1-B6、静默错误 C1-C7、性能/健壮性 D1-D5 中可修项）**——
  - **A1 timetz 时区修复**：此前按 PG 语义推导符号，openGauss 实测（timetz_send 权威字节）磁盘 zone 与 PG **符号相反**（`'10:00:00-05'` 磁盘存 `+18000` 秒、`'-05:30'` 存 `+19800`）；且整点格式省略 `:00`。修复后 `'10:00:00-05'`/`'+05'`/`'-05:30'` 与 openGauss `::text` 输出逐值一致。
  - **A2 数组 NULL 语义修复**：数组元素字面量 `'NULL'` 此前导出为裸 `NULL`（导入后变 SQL NULL 元素）；现区分两义——字面量 `"NULL"`（带引号，`{"NULL",...}`）与 SQL NULL 元素（哨兵 `\x00GN\x00`，裸 `NULL`，`{"x,y",NULL}`），与 openGauss array_out 逐值一致。
  - **B1 CLI 缺参越界**：`--schema/--datadir/-o/--limit/--delimiter/--fields/--encoding/--catalog-json/--table-name/--parallel` 作末尾参数时 `args[i]` 越界 panic → 统一 `argVal` 校验，缺参报错退出（exit 2）。
  - **B2 meta.json 类型断言**：`loadCatalogJSON` 对 `map[string]interface{}` 断言无保护，坏 meta.json panic → 断言失败条目跳过。
  - **B3 varlena 越界**：`ExtractFieldsDirect` 普通 varlena 路径（1B/4B total 与 External 18B）未校验 `offset+total`，损坏数据越界 panic → 越界截断返回（同 CalculateTupleSize 语义），该行解码为缺失列，不崩溃。
  - **B4 decodeBit / B5 decodeArray / B6 decodeRange**：位串位数、数组 NULL 位图、range 定长子类型切片均补越界保护，损坏数据返回空串/`{}`/降级文本，不 panic。
  - **C1 safeElemText**：recover() 丢弃返回值致 `__ARRAY_CORRUPT__` 分支死代码 → recover 捕获错误返回 err。
  - **C2 坏页丢行**：Phase1 发现有效行即跳过 Phase2，损坏页（无有效 ItemId）行被静默丢弃 → `iterPagesRange` 记录坏页，Phase1 后对坏页做数据区扫描补漏（串行与并行路径均实现；并行改为段内内联收集坏页，消除共享切片并发写竞争）。实测破坏第 1/100 页页头 20000 行全量导出不 panic 不丢行。
  - **C3 setval 空表错位**：空表此前 `setval(seq,1,true)` 使下一个 id=2 → 空表改 `is_called=false`（下一个 id=1，与 serial 起始一致）；非空表 `setval(MAX,true)` 不变。
  - **C4 批量缺文件**：`--all-tables/--tables/--schema` 批量导出时数据文件缺失（已 VACUUM FULL 换文件等）此前静默产出空文件 → os.Stat 缺失跳过并 stderr warning。
  - **C5 decodeMoney MinInt64**：取负溢出（-92233720368547758.08 误导出）→ 特判按 PG money_out 返回 `-92233720368547758.08`。
  - **C6 decodeTsquery**：`\` 双转义与 openGauss tsquery_out 不一致 → 仅转义单引号。
  - **C7 decodeTsvector**：非法 UTF-8 词静默 `\ufffd` → `utf8.ValidString` 检查，非法词输出 `\x`+hex 标记。
  - **D1 双遍历**：`--sql`/`--data` 各自执行两次（输出 + 计行数）、`--ddl` 单独白跑整表 ToSQL → 单次调用，nRows 取自调用结果（`--ddl` 单独不再白跑数据遍历）。
  - **D2 relmapper 滞后**：仅 LoadIndexes 修过 pg_class 实时回查 → `SysFilePath` 泛化：relmapper 命中后仍以 pg_class.relfilenode 为准（进程内 rfnClassCache 缓存避免批量每表全扫），LoadEnumMap/LoadDBNames/LoadAttrDefaults/LoadConstraints 等全部受益。
  - **D5 attrFields boolf**：`fields[i]` 空切片 panic（被 defer 吞后列静默丢失）→ len 检查。
  - **回归**：openGauss 6.0.6 实测——timetz/数组 NULL 与 `::text` 权威输出逐值一致；t_big 20000 行破坏第 1/100 页不 panic 不丢行；坏 varlena 截断不崩溃；CLI 缺参 exit 2；坏 meta.json 安全退出；正常数据 SQL/CSV/DDL 与 v0.2.7 **逐字节一致（md5 相同）**无回归；12 正式版全功能回归详见 REGRESSION-GO.md。
- **v0.2.7（2026-10-08）**：**修复 DDL 索引导出重复缺陷**——v0.2.6 的 `--ddl` 增强在导出 `UNIQUE` 约束时，约束已内联进 `CREATE TABLE ... CONSTRAINT "x" UNIQUE (...)`（该约束会隐式创建同名索引），导出器却又额外 emit 一条独立 `CREATE UNIQUE INDEX "x"`，导致严格导入（`ON_ERROR_STOP=1`）在该索引处 abort：`ERROR: relation "x" already exists`（典型样本 `t_ddl_enhance_uniq_col_key`）。根因：索引导出只过滤了 `indisprimary`（PK backing），未区分"约束 backing 索引"（pg_constraint.conindid 指向、contype in p/u）与"独立二级索引"（pg_constraint 无记录）。修复（internal/catalog/catalog.go）：`constraintFields` 提取 conindid（pg_constraint 布局字段 8），`ConstraintInfo` 新增 `Conindid`；新增 `loadBackingIndexOids`，`LoadIndexes` 发射 `CREATE [UNIQUE] INDEX` 时跳过其 OID 被 contype in ('p','u') 约束 conindid 引用的索引——PK/UNIQUE 约束内联声明后不再重复建索引，纯独立二级索引（含非约束唯一索引，如 `t_new_types_u1`）照常导出。实测 6.0.6 `t_ddl_enhance` 不再有重复 `CREATE UNIQUE INDEX`，完整 SQL 导入新库 `ON_ERROR_STOP=1` 零报错逐值一致；无约束/无索引表 `t_all_types` 与基线（md5 `dc1154c2825ebfe37381bf4dbea5395c` / 9901B）逐字节一致（无回归）。
- **v0.2.6（2026-10-08）**：**对齐 pg2sql-go 五项功能补齐（差异清单 1/2/3/4/5）**——
  ①**类型解码补全**（internal/types）：新增 range 全系（int4/num/ts/tstz/date/int8 range，按 openGauss 磁盘格式解码边界/empty）、tsvector（WordEntry 权重位）、tsquery（QueryItem NOT/AND/OR/PHRASE）、几何 7 型（point/lseg/path/box/polygon/line/circle）、txid_snapshot（openGauss OID=2970 与 PG 5030 不同，布局 [nxip][xmin8][xmax8][extra][xip8]，经 txid_snapshot_send 权威校准）、reg* 系列（regclass/regproc/regtype 等，输出 OID 数字与 pg2sql 一致可逆导入）、xml（剥 4B 类型标记）、macaddr8/pg_lsn（防御保留，openGauss 无此类型）；实测 range/tsvector/几何/xml/txid_snapshot 与 gsql 输出逐值一致，SQL 导入新库 6/6 零差异。
  ②**DDL 序列 + setval 同步**（internal/catalog + main.go）：新增 pg_index 解析（openGauss pg_index 目录 OID=2610、列序 indisunique@10/indisprimary@11 与 PG 不同，indkey 用 varlena 定位法兼容布局差异）；`--ddl` 在 CREATE TABLE 前输出 `CREATE SEQUENCE IF NOT EXISTS`（被 DEFAULT nextval 引用的序列，按当前 schema 补全裸名），CREATE TABLE 后输出非主键 `CREATE INDEX`/`CREATE UNIQUE INDEX`（表达式/部分索引跳过）；`--sql` 数据尾部追加 `SELECT setval(...)` 序列同步（导入后自增从最大值继续，实测新插入 id 从 max+1 起）。
  ③**新增 `--limit`/`--delimiter` 参数**：`--limit N` 只导出前 N 行；`--delimiter STR` 自定义 CSV 分隔符（默认 `,`，与 COPY DELIMITER 兼容）。
  ④⑤**批量导出与列表参数对齐 pg2sql**：废弃半成品 `--export-db`，改为 `--tables`（用户表）/`--all-tables`（含系统 schema）/`--schema`（schema 过滤，逗号多值，独立触发批量）；批量模式位置参数为数据库目录、必须 `-o` 目录 + `--sql`/`--data`/`--ddl` 之一，纯 DDL 输出后缀 `.ddl`；`--list-tables-db` 改为只列用户对象（过滤 pg_*/dbe_*/information_schema/coverage/db4ai/snapshot 系统 schema），新增 `--list-tables-all` 列全部对象（均带 schema 前缀）。
  回归：真实 openGauss 6.0.6 集群新类型造数导出-导入闭环通过；12 个正式版本全功能回归见 `REGRESSION-GO.md`。**已知边界**：openGauss 无 line/pg_lsn/macaddr8 类型（解码器保留防御）；reg* 输出 OID 数字（对齐 pg2sql 语义，`'数字'::regclass` 可逆）。
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
