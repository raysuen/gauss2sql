# gauss2sql-go

> English | [中文](README.md)

> Author: raysuen

> Current version: **v0.2.21** (see [Changelog](#5-changelog))

> A Go implementation that parses openGauss (and openGauss-derived) data-directory heap files offline and exports them as SQL / CSV / DDL / metadata JSON.

gauss2sql-go is a **full Golang rewrite** of gauss2sql (Python edition), feature-equivalent: it reads heap files directly from the openGauss data directory without any online service, and exports database objects and restores data.

- **Zero third-party runtime dependencies**: pure Go standard library (since v0.1.7 `golang.org/x/text` is used for GBK/GB18030 transcoding); `go build` produces a single static binary (~3.2 MB)
- **Byte-for-byte identical to the Python edition**: DDL / SQL / CSV / meta.json / `--parallel` output all match
- **Full version coverage**: 12 released openGauss versions (5.0.0 – 6.0.6, x86_64) all pass full-function regression; derived distribution MogDB 5.0.9 passes 18/18
- **Full type support**: int/float/numeric/text/varchar/char/bool/date/time/timestamp/uuid/inet/bit/varbit/money/bytea/jsonb/xid/tid/enums/arrays (with NULL elements)/Chinese column names, plus advanced openGauss types: range family (int4range/numrange/daterange/tsrange etc.)/tsvector/tsquery/geometric (point/lseg/path/box/polygon/circle)/txid_snapshot/reg* family/xml

---

## 1. Features

- `--list-db`: list all databases in the data directory (OID → name mapping; since v0.2.16, appends the directory path of each database on independent tablespaces in extra lines)
- `--db-id OID` (v0.2.16): combined with `--datadir`, locates a single database directly (`<datadir>/base/<OID>`), driving `--list-tables-db`/`--table-name`/`--schema`/`--tables`; coexists with positional arguments, backward compatible
- **Independent tablespace auto-discovery** (v0.2.16): resolves real paths of tables and TOAST files in independent tablespaces through `pg_tblspc` symlinks; no manual path needed; pg_default behavior unchanged
- `--list-tables-db`: list user objects of a database directory (tables/indexes/sequences/TOAST/views; system schemas filtered by default; since v0.2.16 includes tables on independent tablespaces, forward scan of pg_class)
- `--list-tables-all`: list all objects of a database directory (including system schemas)
- `--export-meta`: export metadata JSON (all table structures, used by `--catalog-json`)
- `--ddl --sql`: export table DDL + INSERT data SQL (DDL includes column DEFAULTs, table-level PRIMARY KEY / UNIQUE / CHECK constraints, table/column COMMENT, preceding `CREATE SEQUENCE`, non-PK unique indexes, trailing setval sequence sync, since v0.2.6)
- `--data --output`: export CSV data (`--delimiter` custom separator, `--limit` row cap)
- `--tables / --all-tables / --schema`: batch export — positional arg is the database directory, requires `-o` directory plus one of `--sql`/`--data`/`--ddl`; one `<schema>.<object>.<sql|csv|ddl>` file per table (since v0.2.18 partition parents expand automatically; `<schema>.<parent>.<ext>` contains all sub-partition rows)
- `--parallel N`: parallel export; output byte-for-byte identical to serial
- `--deleted / --only-deleted`: export (export only) deleted rows (ctid locating supported)
- `--count`: count rows only
- `--fields / --header / --encoding`: field filter / header / charset decoding (UTF8/GBK/GB18030/LATIN1 etc.)
- `--catalog-json --table-name`: export by table name against metadata JSON (including partition sub-partitions)
- `--table-name` direct lookup export (since v0.2.12): when the positional arg is the **database directory**, looks up pg_class/pg_namespace/pg_attribute by name without `--catalog-json`; supports `schema.table` and bare names (bare requires global uniqueness, errors on multi-schema duplicates); partition parents auto-expand all sub-partitions (sub-partition bytes equal the concatenation of partition files)

### Large-table streaming export and multi-segment files (since v0.2.13; memory-peak fix in v0.2.14)

- **Streaming (optimization A)**: chunked reads (512 pages = 4 MB window per chunk) + streaming write after each row, replacing the old "read whole file + concatenate all text" — the old path needed ~3.3 GB+ (OOM/swap near host memory limit); since v0.2.14 serial uses a synchronous ordered loop and parallel uses a bounded-window dispatcher. **Measured serial peak 26–28 MB, `--parallel 4` peak 140 MB** (full export of an 1.8M-row, 1.47 GB table)
- **Chunk-level parallel parse + escape (optimization B)**: under `--parallel N` each worker independently does "chunk parse → row escape → text emit", merged by chunk order, **byte-for-byte identical** to serial; CPU-bound escaping no longer limited to one thread
- **Automatic >1 GiB multi-segment support**: openGauss splits a table file over 1 GiB (main file + `.1`/`.2`..., page numbers continue globally). Since v0.2.13 all segments are detected and exported in order — **fixes the old silent data loss from reading only the main segment**. On a 1.8M-row 1.47 GB table (main 1 GiB + `.1` 382 MB), `--count` matches the real row count exactly

### Constant memory: the core benefit of streaming (since v0.2.14)

**Principle**: export memory is independent of table size, driven only by parallelism. The streaming core processes data in a **fixed window** — each chunk is a fixed 512-page (4 MB read window); serial mode is a synchronous ordered loop (only 1 chunk in memory at a time, zero goroutines, zero backlog); parallel mode dispatches a bounded window (in-flight ≤ workers×2 chunks). A chunk is written and released immediately after processing; **peak memory = O(window × single chunk)**, never grows with file size.

**Measured comparison** (openGauss 6.0.6, 1.8M-row 1.47 GB table, 4 cores):

| Version | Mode | Peak RSS | Notes |
|---|---|---|---|
| v0.2.12 (old) | Read whole file + concatenate | ~4–5× output size (1 GB table needs ~3.3 GB+) | OOM/swap near host memory, cannot export |
| v0.2.13 | Streaming but fan-out | 3.26 GB | all chunk goroutines read at once; 3× OOM on 3 GB env |
| **v0.2.14** | Serial streaming | **28 MB (constant)** | 1.8M rows exported rc=0 |
| **v0.2.14** | `--parallel 4` streaming | **140 MB (constant)** | byte-for-byte identical to serial |

**Large-table extrapolation** (constant memory, independent of size):

| Table size | Serial | parallel 4 | Notes |
|---|---|---|---|
| 5 GB | 28 MB | 140 MB | constant |
| 10 GB | 28 MB | 140 MB | constant |
| 20 GB | 28 MB | 140 MB | constant |
| 30 GB | 28 MB | 140 MB | constant |
| 50 GB | 28 MB | 140 MB | constant |

**Practical meaning**:

- A 50 GB table needs only **~140 MB (parallel 4)** peak; an ordinary 2–4 GB RAM server carries exports of any scale, no 4–5× output-size reservation
- `--parallel N` memory scales roughly linearly: **≈35 MB × N** (measured 4 workers = 140 MB); pick parallelism from available RAM
- The only cost growing with the table is **disk IO and time** (linear); memory is no longer the bottleneck

### `--jobs` vs `--parallel` resource estimation (since v0.2.20)

Batch export has two orthogonal levels of parallelism: `--parallel N` is **intra-table** page-level parallelism (inner), `--jobs M` is **inter-table** worker count (outer); they multiply.

**Memory formula**:

```
Total memory ≈ shared catalog cache (~15 MB) + jobs × per-table streaming peak
```

Per-table streaming peak grows with `--parallel` — each inner worker adds its own page-parse/escape buffer; `--jobs` runs multiple tables in parallel, each holding one per-table peak.

**Measured anchors** (openGauss 6.0.6, 1.8M-row 1.47 GB table):

- Single table serial (parallel=1): ~**28 MB**
- Single table `--parallel 4`: ~**140 MB**

Linear interpolation: parallel=1 → 28 MB, parallel=2 → ~65 MB, parallel=4 → 140 MB.

**Estimated peak memory (MB, planning estimate)**:

| jobs \ parallel | parallel=1 | parallel=2 | parallel=4 |
|---|---|---|---|
| jobs=1 | ≈43 | ≈80 | ≈155 |
| jobs=4 | ≈127 | ≈275 | ≈575 |
| jobs=8 | ≈239 | ≈535 | ≈1135 |
| jobs=16 | ≈463 | ≈1055 | ≈2255 |

Cell = 15 MB shared catalog cache + jobs × per-table peak (above).

> Actual usage depends on row width, TOAST, corrupt pages and concurrent IO; this table is a **planning estimate**; run large batch exports in background and watch RSS.

**CPU formula**:

```
Total concurrent workers ≈ jobs × parallel
```

Recommended cap: physical cores × 2; beyond that context switching and IO contention slow it down.

**Recommended profiles**:

- **3 GB RAM sandbox/container**: `jobs=4`, `parallel=2` (~275 MB peak, headroom)
- **8 GB production**: `jobs=8`, `parallel=4` (~1.1 GB peak)

**Other v0.2.20 flags**: `--batch-rows N` adds almost no memory (accumulate N rows then flush), but sharply reduces INSERT statement count; `--report PATH` only adds one SHA-256 per table, negligible overhead.

### Corrupt-block robustness (since v0.2.8)

Not a simple skip: three-level, best-effort recovery, never panics:

| Corruption level | Detection | Action | Effect |
|---|---|---|---|
| **Corrupt page header** (whole page untrustworthy) | Invalid pd_lower/pd_upper (`HasValidLayout=false`) | Skip the page via standard ItemId walk and record it, then **data-area scan salvage** (bypass untrustworthy header/ItemId and find tuples directly) | Corrupt-page rows recovered; breaking any page header on a healthy 20000-row table exports all rows, no panic, **no row loss** |
| **Single bad row** (tuple header corrupt) | tuple header parse failure | Drop only that row, `continue` to the rest | Other rows export normally, no spread |
| **Field-level corruption** (varlena out-of-range / bitstring / array bitmap / range out-of-range) | decode boundary checks | out-of-range varlena truncates the row (single-row drop); out-of-range bitstring/array/range emits empty or degraded text | row kept or single-row dropped, **never crashes** |

- Corrupt meta.json / missing CLI argument: safe error exit
- Corruption affects only that page/row; the rest of the table stays byte-for-byte correct (no regression)

### Supported openGauss on-disk formats (reverse-engineered and ported)

| Format | Notes |
|---|---|
| Page layout | 8 KB, version 6, 24 B header |
| HeapTuple | t_xmin/t_xmax 8B xid, t_cid, t_hoff; system-catalog row OID at t_hoff-4 |
| Three-level catalog location | standard filename → relmapper → pg_class relfilenode (pg_enum/pg_namespace not in relmapper) |
| varlena | 4B/1B header; TOAST Light inline [4B header][4B rawsize][PGLZ stream] (compress flag low 2 bits ==2); PGLZ decompress |
| Out-of-line TOAST | 18B pointer [rawsize][extsize][valueid][toastrelid]; toastrelid mapped to physical file via pg_class |
| jsonb JEntry | interleaved key/value, low 28 bits = absolute end offset, type bits (container/false/null/true...), container/numeric start INTALIGN4 aligned |
| tid | [hi 2B][lo 2B][pos 2B] all little-endian, blk=(hi<<16)\|lo |
| Partitioned tables | parent relfilenode=0; sub-partitions in pg_partition; backtrack parentid → pg_class parent → inherit column definition |

---

## 2. Build

Requirements: Go ≥ 1.23.

```bash
# Build on host (linux amd64)
go build -o gauss2sql-go .

# linux x86_64 cross-compile
GOOS=linux GOARCH=amd64 go build -o gauss2sql-go-linux-amd64 .

# linux aarch64 (ARM64) cross-compile
GOOS=linux GOARCH=arm64 go build -o gauss2sql-go-linux-arm64 .
```

> Cross-compilation needs no target toolchain; `CGO_ENABLED=0` defaults to static linking; the binary runs directly on the target Linux platform.

---

## 3. Usage

```
gauss2sql-go <data file or data directory> [options]
```

Common examples:

```bash
# List all databases
gauss2sql-go --datadir /data/openGauss --list-db

# List user objects / all objects of a database
gauss2sql-go base/16388 --list-tables-db
gauss2sql-go base/16388 --list-tables-all

# Batch export user tables (positional = database dir; requires -o dir + --sql/--data/--ddl)
gauss2sql-go base/16388 --tables --sql -o /tmp/out
gauss2sql-go base/16388 --tables --schema app,public --sql -o /tmp/out
gauss2sql-go base/16388 --all-tables --ddl -o /tmp/ddl   # system-schema DDL only, .ddl suffix

# Export metadata JSON
gauss2sql-go --datadir /data/openGauss --export-meta -o meta.json

# Export a table's DDL + SQL
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --ddl --sql

# Export CSV (explicit file)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --data --output t_all.csv

# Export CSV (auto-named to directory: /tmp/app.t_all_types.csv)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --data --header -o /tmp/

# No -o: write to stdout (redirectable)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_all_types --sql > app.t_all_types.sql

# Parallel export (4 workers)
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_big --sql --parallel 4

# Count / export deleted rows
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_del --deleted --count
gauss2sql-go base/16388/16414 --catalog-json meta.json --table-name app.t_del --only-deleted --sql
```

### CLI reference

| Flag | Description |
|---|---|
| `--datadir` | openGauss data directory (with --list-db etc.) |
| `--list-db` | list databases |
| `--list-tables-db` | list user objects of a database (tables/indexes/sequences/TOAST/views, system schemas filtered) |
| `--list-tables-all` | list all objects (including system schemas) |
| `--export-meta` | export metadata JSON |
| `--tables` | batch-export all user tables (positional = database dir; needs `-o` dir + `--sql`/`--data`/`--ddl`) |
| `--all-tables` | batch-export all tables (including system schemas) |
| `--schema` | batch-export filtered by schema (comma-separated, equivalent to `--tables --schema`) |
| `--ddl` | CREATE TABLE DDL (DEFAULTs/constraints/comments/CREATE SEQUENCE/non-PK indexes) |
| `--sql` | INSERT data SQL (trailing setval sync) |
| `--data` | CSV output (needs `--output`; `--delimiter` custom separator) |
| `--output` / `-o` | file or directory; **omitted → stdout**; directory (trailing `/` or existing) auto-generates `<path>/<schema>.<object>.<sql|csv|ddl>`; extension-less path appends `.sql/.csv` |
| `--parallel N` | intra-table page-level parallel workers (inner, default 1 serial; multiplies orthogonally with `--jobs`) |
| `--jobs M` | inter-table batch workers (outer, v0.2.20, default 1 serial; see resource estimation above) |
| `--batch-rows N` | multi-value INSERT: combine N rows into one INSERT (v0.2.20, default 0=off, one row per INSERT, byte-identical to old) |
| `--report PATH` | per-table stats JSON after export (v0.2.20, off by default; schema/name/oid/rows/bytes/sha256/duration_ms/corrupt_pages) |
| `--limit N` | export first N rows only |
| `--delimiter STR` | CSV field separator (default `,`) |
| `--deleted` | include deleted rows |
| `--only-deleted` | export deleted rows only |
| `--count` | count rows only |
| `--fields` | select fields to export |
| `--header` | CSV header row |
| `--encoding` | decode encoding (auto-detect; fallback `UTF8`/`GBK`/`GB18030`/`LATIN1`/`SQL_ASCII`; output always UTF-8) |
| `--verbose` | print export info to stderr (structure/TOAST/path/rows/elapsed) |
| `--catalog-json` | metadata JSON path |
| `--table-name` | export by table name (schema.table supported) |
| `--version` | version info |

---

## 4. Testing & Regression

Automated framework: `run_regression_go.sh` (with `test_data.sql` seed script).

| Test line | Result | Report |
|---|---|---|
| All openGauss versions (5.0.0–6.0.6, 12 releases) | 12/12 ALL PASS | `REGRESSION-GO.md` |
| Python-edition byte consistency (5.0.5 baseline diff) | zero difference | `REGRESSION-GO.md` |
| 50-column bulk (50,052 rows, mixed CN/EN + full special-char set) | ALL PASS | `REGRESSION-GO.md` |
| Derived distribution MogDB 5.0.9 | 18/18 ALL PASS | `REGRESSION-DERIVED.md` |

---

## 5. Changelog

- **v0.2.21 (2026-10-15)**: GBK/GB18030 end-to-end fix — jsonb string leaves use codec / DDL fragments use codec / import script sets UTF8 encoding.
  - C1 jsonb string leaves go through the global codec (`jsonbScalarToText`): measured on a 6.0.5 GBK database that jsonb container string leaves are stored as GBK bytes (same as text columns); the earlier wrong assumption "jsonb is always UTF-8" forced UTF-8 decode and produced latin-1 mojibake. UTF-8 databases pass through unchanged.
  - C2 confirmed escape order (decode to UTF-8 runes first, then `'`→`''`, `\`→`\\`, control chars `\xXX`); added unit tests.
  - C3 Chinese DDL fragments (column defaults `pg_attrdef`, comments `pg_description`, CHECK expressions `pg_constraint.conbin`) go through the codec.
  - C4 generated `import.sh` runs `\encoding UTF8` before each `gsql -f`.
  - Verified on openGauss 5.0.0 / 6.0.5 / 6.0.6: utf8.Valid 100%, jsonb Chinese correct, TOAST large fields, bpchar, empty-string vs NULL, GB18030 extension chars 㐀㑭㠇 all pass; zero latin-1 mojibake signatures; cross-version INSERT bytes match 6.0.5 baseline.
  - UTF-8 baseline md5 unchanged across the 12 catalogs; `go test ./...` all PASS.

- **v0.2.20 (2026-10-14)**: Four batch-export enhancements, all off by default:
  - `--batch-rows N`: multi-value INSERT, N rows per statement (default 0 = off, old byte output).
  - `--jobs M`: inter-table parallel workers (outer), orthogonal with `--parallel`.
  - `--report PATH`: per-table stats JSON (schema/name/oid/rows/bytes/sha256/duration_ms/corrupt_pages).
  - Auto-generated `import.sh`: sorted gsql sequence, DDL before data, `bash -n` validated.
  - Compatibility: off by default; unchanged output when not enabled.

- **v0.2.19 (2026-10-13)**: Fix sub-partition tablespace resolution — resolve child files in independent tablespaces via the sub-partition's own `pg_partition.reltablespace` (field 7, verified on 6.0.6 and 5.0.5); fall back to parent tablespace when 0; 4 tablespace combinations tested on 6.0.6, all row counts and byte comparisons correct.

- **v0.2.18 (2026-10-13)**: Partition coverage — batch export expands partition parents (openGauss traditional parents have relkind='r' in pg_class, not PG 'p'; new PartitionParentOids set from pg_partition); `--table-name` exports a single sub-partition by name; 2999-row RANGE partition tested on 6.0.6.

- **v0.2.17 (2026-10-13)**: Fix intermittent parallel-chunk deadlock (bounded-window dispatch moved before blocking receive); add physical-file existence filter to `--list-tables-all`/`--list-tables-db` (drop view/shared-table/composite ghost paths).

- **v0.2.16 (2026-10-13)**: Independent tablespace auto-discovery via pg_tblspc symlinks; new `--db-id OID`; multi-line tablespace paths in `--list-db`; forward pg_class scan; `pkg_*` system-schema filter.

- **v0.2.15 (2026-10-10)**: Documentation round — added constant-memory streaming section; no code change.

- **v0.2.14 (2026-10-10)**: Fix runChunked fan-out OOM (serial synchronous loop ~26 MB, parallel bounded window ~140 MB); fix corrupt-page salvage duplicate rows; 1.8M-row 1.47 GB table fully exported.

- **v0.2.13 (2026-10-09)**: A+B large-table optimization (streaming + chunk-parallel parse/escape); >1 GiB multi-segment support (fixes silent data loss on the `.1` segment).

- **v0.2.11 / v0.2.10 / v0.2.9 (2026-10-08)**: Documentation rounds for the three-level corrupt-block section; no code change.

- **v0.2.8 (2026-10-08)**: Deep review, 20 defects fixed: timetz timezone sign, array NULL semantics, CLI arg bounds, meta.json type assertion, varlena/bit/array/range bounds, corrupt-page salvage, empty-table setval, missing-file warning, money MinInt64, tsquery/tsvector escaping, single-pass traversal, relmapper staleness, bool-field slice panic.

- **v0.2.7 (2026-10-08)**: Fix duplicate DDL index emission — skip constraint-backing indexes (conindid from pg_constraint) when emitting standalone indexes.

- **v0.2.6 (2026-10-08)**: Type decode completion (range/tsvector/tsquery/geometric/txid_snapshot/reg*/xml); DDL sequence + setval sync; new `--limit`/`--delimiter`; batch export and list flags (`--tables`/`--all-tables`/`--schema`).

- **v0.2.5 / v0.2.2 / v0.2.15**: Documentation and help-text sync rounds.

- **v0.2.4 (2026-09-29)**: Fix DDL-enhancement bugs on real clusters: pg_attrdef OID 2604, pg_constraint layout (drop phantom conparentid, add 6.0.x columns), varlena header strip for defaults/comments/constraints.

- **v0.2.3 (2026-09-29)**: DDL enhancement: column DEFAULTs, PRIMARY KEY/UNIQUE/CHECK, COMMENT ON TABLE/COLUMN; meta.json oid + primary_key.

- **v0.2.1 (2026-09-29)**: Fix `--export-meta` empty enums; `--catalog-json` rebuilds enums.

- **v0.2.0 (2026-09-29)**: `--catalog-json` closed loop: toastrelid/relkind/type_names/enums; corrupt varlena decode no longer panics.

- **v0.1.9 (2026-09-29)**: `--parallel` truly active; TOAST whole-file cache; fix roleNameMap race.

- **v0.1.8 (2026-09-29)**: Strict empty-string `''` vs NULL; CSV empty field vs `\N`.

- **v0.1.7 (2026-09-29)**: GBK/GB18030 transcoding via golang.org/x/text; correct openGauss encoding enums (GBK=6, UTF8=7, LATIN1=9, GB18030=36).

- **v0.1.6 / v0.1.5 / v0.1.4 / v0.1.3**: stdout default, `-o` auto-naming, `--verbose`, `--help`, output-path fixes.

- **v0.1.2**: Packaging — README, MIT LICENSE; amd64/arm64 cross-compile.

- **v0.1.1**: Fix `--list-db` unresolved database names; 12/12 regression.

- **v0.1.0**: Full Golang rewrite of the Python edition's 8 modules and on-disk format parsing.

---

## 6. License

MIT License, see [LICENSE](LICENSE).

Copyright (c) 2026 raysuen
