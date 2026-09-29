#!/bin/bash
# ============================================================
# gauss2sql 全版本回归测试框架 v2
# Author: raysuen
# 用法: ./run_regression.sh <version> <tarball_path> <port>
# ============================================================
set -u

VERSION="$1"
TARBALL="$2"
PORT="$3"

OG_BASE="/home/user/Doubao/chats/38443863261710082/_og"
GAUSS2SQL="/home/user/Doubao/chats/38443863261710082/gauss2sql"
TEST_DATA_SQL="$GAUSS2SQL/test_data.sql"
LIB_AIO_SOURCE="/home/user/Doubao/chats/38442878699045890/kb8c9b14/lib/libaio.so.1"

VER_DIR="og$(echo $VERSION | tr -d '.')"
VER_NODOTS="$(echo $VERSION | tr -d '.')"
GAUSSHOME="$OG_BASE/$VER_DIR"
OG505_LIB="$OG_BASE/og505/lib"
PGDATA="$OG_BASE/data_v${VERSION}"
GAUSSLOG="$OG_BASE/log_v${VERSION}"
OUT_DIR="$OG_BASE/regress_go_${VERSION}"
RESULT_FILE="$OUT_DIR/result.txt"
SRC_PORT="$PORT"

mkdir -p "$OUT_DIR"
FAIL_COUNT=0

log() { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$RESULT_FILE"; }
pass() { echo "PASS: $*" | tee -a "$RESULT_FILE"; }
fail() { echo "FAIL: $*" | tee -a "$RESULT_FILE"; FAIL_COUNT=$((FAIL_COUNT+1)); }

# 获取表的 relfilenode
get_relfilenode() {
  local schema_table="$1"
  local schema="${schema_table%%.*}"
  local table="${schema_table##*.}"
  gsql -d appdb -p "$SRC_PORT" -t -c "SELECT relfilenode FROM pg_class WHERE relname='$table' AND relnamespace=(SELECT oid FROM pg_namespace WHERE nspname='$schema');" 2>/dev/null | tr -d ' '
}

# ============================================================
# 阶段1: 部署实例
# ============================================================
log "===== 版本 $VERSION 回归测试开始 ====="
log "tarball: $(basename $TARBALL), port=$SRC_PORT"

# 1.1 解压
if [ ! -d "$GAUSSHOME/bin" ]; then
  log "解压 $TARBALL -> $GAUSSHOME"
  mkdir -p "$GAUSSHOME"
  if echo "$TARBALL" | grep -q '\.tar\.gz$'; then
    tar -xzf "$TARBALL" -C "$GAUSSHOME" 2>&1 | tail -3
  else
    tar -xjf "$TARBALL" -C "$GAUSSHOME" 2>&1 | tail -3
  fi
  # 处理内层目录
  if [ ! -d "$GAUSSHOME/bin" ]; then
    inner=$(ls -d "$GAUSSHOME"/*/ 2>/dev/null | head -1)
    if [ -n "$inner" ] && [ -d "${inner}bin" ]; then
      log "移动内层目录内容"
      mv "$inner"* "$GAUSSHOME/" 2>/dev/null
      rmdir "$inner" 2>/dev/null
    fi
  fi
else
  log "已解压，跳过"
fi

if [ ! -f "$GAUSSHOME/bin/gaussdb" ]; then
  fail "解压失败：找不到 gaussdb"
  exit 1
fi
pass "解压完成"

# 1.2 依赖修复
cd "$GAUSSHOME/lib"
[ -f /lib/x86_64-linux-gnu/libreadline.so.8.1 ] && ln -sf /lib/x86_64-linux-gnu/libreadline.so.8.1 libreadline.so.6 2>/dev/null
[ -f /lib/x86_64-linux-gnu/libncurses.so.6 ] && ln -sf /lib/x86_64-linux-gnu/libncurses.so.6 libncurses.so.5 2>/dev/null
[ -f /lib/x86_64-linux-gnu/libncursesw.so.6 ] && ln -sf /lib/x86_64-linux-gnu/libncursesw.so.6 libtinfo.so.5 2>/dev/null
[ -f "$LIB_AIO_SOURCE" ] && cp -f "$LIB_AIO_SOURCE" libaio.so.1 2>/dev/null
# 从 5.0.5 复制缺失的依赖库（OpenSSL、krb5、pcre 等）
if [ -d "$OG505_LIB" ]; then
  for so in "$OG505_LIB"/*.so*; do
    bn=$(basename "$so")
    if [ ! -e "$GAUSSHOME/lib/$bn" ]; then
      cp -f "$so" "$GAUSSHOME/lib/$bn" 2>/dev/null
    fi
  done
fi
# OpenSSL 1.0 兼容软链（CentOS 构建需要 libssl.so.10 / libcrypto.so.10）
[ -f libssl.so.1.0.2k ] && ln -sf libssl.so.1.0.2k libssl.so.10 2>/dev/null
[ -f libcrypto.so.1.0.2k ] && ln -sf libcrypto.so.1.0.2k libcrypto.so.10 2>/dev/null
pass "依赖修复完成"

# 1.3 环境变量
export GAUSSHOME
export LD_LIBRARY_PATH="$GAUSSHOME/lib:$GAUSSHOME/lib/postgresql"
export PATH="$GAUSSHOME/bin:$PATH"

# 1.4 初始化数据目录
if [ ! -d "$PGDATA/base" ]; then
  log "初始化数据目录"
  rm -rf "$PGDATA"
  mkdir -p "$PGDATA"
  if [ -f "$GAUSSHOME/bin/gs_initdb" ]; then
    gs_initdb -D "$PGDATA" --nodename=regress_${VER_NODOTS} -U user 2>&1 | tail -5
  fi
  if [ ! -d "$PGDATA/base" ]; then
    log "gs_initdb 未成功，尝试 gaussdb --init"
    gaussdb -D "$PGDATA" --init 2>&1 | tail -5 || true
  fi
fi

if [ ! -d "$PGDATA/base" ]; then
  fail "数据目录初始化失败"
  exit 1
fi
pass "数据目录初始化完成"

# 1.5 配置
rm -f "$PGDATA/postmaster.pid" "$PGDATA/postmaster.pid.lock"
if grep -q "^port" "$PGDATA/postgresql.conf" 2>/dev/null; then
  sed -i "s/^port.*/port = $SRC_PORT/" "$PGDATA/postgresql.conf"
else
  echo "port = $SRC_PORT" >> "$PGDATA/postgresql.conf"
fi
grep -q "^listen_addresses" "$PGDATA/postgresql.conf" 2>/dev/null || echo "listen_addresses = '127.0.0.1'" >> "$PGDATA/postgresql.conf"
echo "local all all trust" > "$PGDATA/pg_hba.conf"
echo "host all all 127.0.0.1/32 trust" >> "$PGDATA/pg_hba.conf"
echo "host all all ::1/128 trust" >> "$PGDATA/pg_hba.conf"
pass "配置完成 (port=$SRC_PORT)"

# 1.6 启动
log "启动实例"
export PGDATA
export GAUSSLOG
mkdir -p "$GAUSSLOG"
nohup gaussdb -D "$PGDATA" > "$OG_BASE/gaussdb_${VERSION}.out" 2>&1 &
sleep 5
for i in $(seq 1 15); do
  gsql -d postgres -p "$SRC_PORT" -c "SELECT 1;" >/dev/null 2>&1 && break
  sleep 2
done

if gsql -d postgres -p "$SRC_PORT" -c "SELECT 1;" >/dev/null 2>&1; then
  pass "实例启动成功"
  gsql -d postgres -p "$SRC_PORT" -c "SELECT version();" 2>&1 | head -2 | tee -a "$RESULT_FILE"
  # 5.0.0+ 要求先设置用户密码才能执行 DDL
  gsql -d postgres -p "$SRC_PORT" -c "ALTER ROLE \"user\" PASSWORD 'Gauss@123';" >/dev/null 2>&1
  log "用户密码已设置"
else
  fail "实例启动失败"
  tail -20 "$OG_BASE/gaussdb_${VERSION}.out" | tee -a "$RESULT_FILE"
  exit 1
fi

# ============================================================
# 阶段2: 造数
# ============================================================
log "----- 造数 -----"
gsql -d postgres -p "$SRC_PORT" -c "DROP DATABASE IF EXISTS appdb;" 2>&1 | tail -1
gsql -d postgres -p "$SRC_PORT" -c "CREATE DATABASE appdb;" 2>&1 | tail -1
gsql -d appdb -p "$SRC_PORT" -c "CREATE SCHEMA IF NOT EXISTS app;" 2>&1 | tail -1

log "执行造数脚本"
gsql -d appdb -p "$SRC_PORT" -f "$TEST_DATA_SQL" > "$OUT_DIR/load_data.log" 2>&1
if [ $? -eq 0 ]; then
  pass "造数成功"
else
  fail "造数失败（部分对象可能已创建）"
  tail -10 "$OUT_DIR/load_data.log" | tee -a "$RESULT_FILE"
fi

# 验证行数
for tbl in "app.t_all_types" "app.t_toast" "app.t_part" "public.jb_test" "public.t_tid" "app.t_simple"; do
  cnt=$(gsql -d appdb -p "$SRC_PORT" -t -c "SELECT count(*) FROM $tbl;" 2>/dev/null | tr -d ' ')
  log "  $tbl: $cnt rows"
done

DB_OID=$(gsql -d appdb -p "$SRC_PORT" -t -c "SELECT oid FROM pg_database WHERE datname='appdb';" 2>/dev/null | tr -d ' ')
DB_DIR="$PGDATA/base/$DB_OID"
log "appdb OID=$DB_OID, dir=$DB_DIR"

# 获取各表 relfilenode
RN_ALLTYPES=$(get_relfilenode "app.t_all_types")
RN_TOAST=$(get_relfilenode "app.t_toast")
RN_PART=$(get_relfilenode "app.t_part")
RN_JBTEST=$(get_relfilenode "public.jb_test")
RN_SIMPLE=$(get_relfilenode "app.t_simple")
RN_TID=$(get_relfilenode "public.t_tid")
log "relfilenode: t_all_types=$RN_ALLTYPES t_toast=$RN_TOAST t_part=$RN_PART jb_test=$RN_JBTEST t_simple=$RN_SIMPLE"

# ============================================================
# 阶段3: gauss2sql 全功能测试
# ============================================================
log "----- gauss2sql 功能测试 -----"
cd "$GAUSS2SQL"

# 3.1 --list-db
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go --datadir "$PGDATA" --list-db > "$OUT_DIR/list_db.txt" 2>&1
if [ $? -eq 0 ] && grep -q "appdb" "$OUT_DIR/list_db.txt"; then
  pass "--list-db"
else
  fail "--list-db"; cat "$OUT_DIR/list_db.txt" | tee -a "$RESULT_FILE"
fi

# 3.2 --list-tables-db (positional arg)
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR" --list-tables-db > "$OUT_DIR/list_tables.txt" 2>&1
if [ $? -eq 0 ] && grep -q "t_all_types\|t_simple" "$OUT_DIR/list_tables.txt"; then
  pass "--list-tables-db"
else
  fail "--list-tables-db"; cat "$OUT_DIR/list_tables.txt" | tee -a "$RESULT_FILE"
fi

# 3.3 --export-meta (positional arg)
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR" --export-meta -o "$OUT_DIR/meta.json" > "$OUT_DIR/export_meta.log" 2>&1
if [ $? -eq 0 ] && [ -f "$OUT_DIR/meta.json" ] && [ $(stat -c%s "$OUT_DIR/meta.json") -gt 1000 ]; then
  tc=$(python3 -c "import json; print(len(json.load(open('$OUT_DIR/meta.json')).get('tables',[])))" 2>/dev/null || echo "?")
  pass "--export-meta (tables=$tc)"
else
  fail "--export-meta"; cat "$OUT_DIR/export_meta.log" | tee -a "$RESULT_FILE"
fi

# 3.4 --ddl --sql (t_all_types, data file path)
log "导出 app.t_all_types (DDL+SQL)"
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_ALLTYPES" --ddl --sql -o "$OUT_DIR/t_all_types.sql" 2>"$OUT_DIR/t_all_types.err"
if [ $? -eq 0 ] && [ -s "$OUT_DIR/t_all_types.sql" ]; then
  ic=$(grep -c "^INSERT" "$OUT_DIR/t_all_types.sql" 2>/dev/null || echo 0)
  pass "--ddl --sql (inserts=$ic)"
else
  fail "--ddl --sql"; cat "$OUT_DIR/t_all_types.err" | tee -a "$RESULT_FILE"
fi

# 3.5 CSV 导出
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_ALLTYPES" --data --output "$OUT_DIR/t_all_types.csv" > "$OUT_DIR/csv.log" 2>&1
if [ $? -eq 0 ] && [ -f "$OUT_DIR/t_all_types.csv" ] && [ $(stat -c%s "$OUT_DIR/t_all_types.csv") -gt 0 ]; then
  pass "--data CSV"
else
  fail "--data CSV"; cat "$OUT_DIR/csv.log" | tee -a "$RESULT_FILE"
fi

# 3.6 --parallel 4 (t_simple)
log "测试 --parallel 4"
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --sql --parallel 4 -o "$OUT_DIR/t_simple_parallel.sql" 2>"$OUT_DIR/parallel.err"
if [ $? -eq 0 ] && [ -s "$OUT_DIR/t_simple_parallel.sql" ]; then
  /home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --sql -o "$OUT_DIR/t_simple_serial.sql" 2>/dev/null
  pc=$(grep -c "^INSERT" "$OUT_DIR/t_simple_parallel.sql" 2>/dev/null || echo 0)
  sc=$(grep -c "^INSERT" "$OUT_DIR/t_simple_serial.sql" 2>/dev/null || echo 0)
  if [ "$pc" = "$sc" ] && [ "$pc" -gt 0 ]; then
    pass "--parallel 4 (rows=$pc, 与串行一致)"
  else
    fail "--parallel 4 (parallel=$pc, serial=$sc)"
  fi
else
  fail "--parallel 4"; cat "$OUT_DIR/parallel.err" | tee -a "$RESULT_FILE"
fi

# 3.7 --count
cnt=$(/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_ALLTYPES" --count 2>&1 | grep -oE '[0-9]+' | head -1)
if [ "$cnt" = "5" ]; then
  pass "--count (=5)"
else
  fail "--count (got=$cnt)"
fi

# 3.8 --deleted --count
delcnt=$(/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --deleted --count 2>&1 | grep -oE '[0-9]+' | head -1)
if [ -n "$delcnt" ] && [ "$delcnt" -gt 0 ] 2>/dev/null; then
  pass "--deleted --count (=$delcnt)"
else
  log "  --deleted --count output: $(/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --deleted --count 2>&1 | head -3)"
  fail "--deleted --count"
fi

# 3.9 --only-deleted --sql
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --only-deleted --sql -o "$OUT_DIR/deleted.sql" 2>"$OUT_DIR/deleted.err"
if [ $? -eq 0 ]; then
  dc=$(grep -c "^INSERT" "$OUT_DIR/deleted.sql" 2>/dev/null || echo 0)
  pass "--only-deleted --sql (deleted_rows=$dc)"
else
  fail "--only-deleted --sql"; cat "$OUT_DIR/deleted.err" | tee -a "$RESULT_FILE"
fi

# 3.10 分区表导出（openGauss 分区父表可能无物理文件，需找子分区）
log "测试分区表导出"
PART_EXPORT_OK=0
if [ -n "$RN_PART" ] && [ -f "$DB_DIR/$RN_PART" ]; then
  /home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_PART" --ddl --sql -o "$OUT_DIR/t_part.sql" 2>"$OUT_DIR/t_part.err"
  [ $? -eq 0 ] && [ -s "$OUT_DIR/t_part.sql" ] && PART_EXPORT_OK=1
fi
if [ $PART_EXPORT_OK -eq 0 ]; then
  # 尝试从 pg_partition 找子分区
  CHILD_RNS=$(gsql -d appdb -p "$SRC_PORT" -t -c "SELECT relfilenode FROM pg_partition WHERE parentid=(SELECT oid FROM pg_class WHERE relname='t_part' AND relnamespace=(SELECT oid FROM pg_namespace WHERE nspname='app')) AND relfilenode>0;" 2>/dev/null | tr -d ' ')
  > "$OUT_DIR/t_part.sql"
  for crn in $CHILD_RNS; do
    if [ -f "$DB_DIR/$crn" ]; then
      /home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$crn" --sql -o "$OUT_DIR/t_part_child_$crn.sql" 2>"$OUT_DIR/t_part_child.err"
      [ -f "$OUT_DIR/t_part_child_$crn.sql" ] && cat "$OUT_DIR/t_part_child_$crn.sql" >> "$OUT_DIR/t_part.sql"
    fi
  done
  if [ -s "$OUT_DIR/t_part.sql" ]; then
    pass "分区表导出 (子分区: $CHILD_RNS)"
    PART_EXPORT_OK=1
  fi
fi
if [ $PART_EXPORT_OK -eq 0 ]; then
  fail "分区表导出 (父表无物理文件，需 gauss2sql 适配 pg_partition)"
  cat "$OUT_DIR/t_part.err" 2>/dev/null | head -3 | tee -a "$RESULT_FILE"
fi

# 3.11 TOAST 表导出
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_TOAST" --sql -o "$OUT_DIR/t_toast.sql" 2>"$OUT_DIR/t_toast.err"
if [ $? -eq 0 ] && [ -s "$OUT_DIR/t_toast.sql" ]; then
  pass "TOAST 表导出"
else
  fail "TOAST 表导出"; cat "$OUT_DIR/t_toast.err" | tee -a "$RESULT_FILE"
fi

# 3.12 jsonb 表导出
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_JBTEST" --sql -o "$OUT_DIR/jb_test.sql" 2>"$OUT_DIR/jb_test.err"
if [ $? -eq 0 ] && [ -s "$OUT_DIR/jb_test.sql" ]; then
  pass "jsonb 表导出"
  # 验证 jsonb 内容
  if grep -q "nested" "$OUT_DIR/jb_test.sql" && grep -q "big" "$OUT_DIR/jb_test.sql"; then
    pass "jsonb 内容正确 (含 nested 和 big)"
  else
    fail "jsonb 内容缺失"
    cat "$OUT_DIR/jb_test.sql" | tee -a "$RESULT_FILE"
  fi
else
  fail "jsonb 表导出"; cat "$OUT_DIR/jb_test.err" | tee -a "$RESULT_FILE"
fi

# 3.13 tid 表导出
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_TID" --sql -o "$OUT_DIR/t_tid.sql" 2>"$OUT_DIR/t_tid.err"
if [ $? -eq 0 ] && [ -s "$OUT_DIR/t_tid.sql" ]; then
  pass "tid 表导出"
else
  fail "tid 表导出"; cat "$OUT_DIR/t_tid.err" | tee -a "$RESULT_FILE"
fi

# 3.14 CLI 冒烟
/home/user/Doubao/chats/38443863261710082/gauss2sql-go/gauss2sql-go "$DB_DIR/$RN_SIMPLE" --data --output "$OUT_DIR/smoke.csv" --fields id,name --header --encoding utf-8 >/dev/null 2>&1
if [ $? -eq 0 ] && [ -f "$OUT_DIR/smoke.csv" ]; then
  pass "CLI --fields/--header/--encoding"
else
  fail "CLI --fields/--header/--encoding"
fi

# ============================================================
# 阶段4: 导入新库 + 逐值比对
# ============================================================
log "----- 导入新库 + 逐值比对 -----"
gsql -d postgres -p "$SRC_PORT" -c "DROP DATABASE IF EXISTS import_test;" 2>&1 | tail -1
gsql -d postgres -p "$SRC_PORT" -c "CREATE DATABASE import_test;" 2>&1 | tail -1
gsql -d import_test -p "$SRC_PORT" -c "CREATE SCHEMA IF NOT EXISTS app;" 2>&1 | tail -1

log "导入 t_all_types"
gsql -d import_test -p "$SRC_PORT" -f "$OUT_DIR/t_all_types.sql" > "$OUT_DIR/import.log" 2>&1
if [ $? -eq 0 ]; then
  pass "导入成功"
else
  fail "导入失败"
  tail -10 "$OUT_DIR/import.log" | tee -a "$RESULT_FILE"
fi

# 逐值比对
log "执行逐值比对"
cat > "$OUT_DIR/compare.sql" << 'EOF'
\pset format unaligned
\pset tuples_only on
SELECT 'count', count(*) FROM app.t_all_types;
SELECT 'sum_c_int4', coalesce(sum(c_int4)::text, 'NULL') FROM app.t_all_types;
SELECT 'max_c_numeric', coalesce(max(c_numeric)::text, 'NULL') FROM app.t_all_types;
SELECT 'max_c_timestamp', coalesce(max(c_timestamp)::text, 'NULL') FROM app.t_all_types;
SELECT 'happy_count', count(*) FROM app.t_all_types WHERE c_mood='happy';
SELECT 'uuid_val', coalesce(c_uuid::text, 'NULL') FROM app.t_all_types WHERE c_uuid IS NOT NULL LIMIT 1;
SELECT 'inet_val', coalesce(c_inet::text, 'NULL') FROM app.t_all_types WHERE c_inet IS NOT NULL LIMIT 1;
SELECT 'bit_val', coalesce(c_bit::text, 'NULL') FROM app.t_all_types WHERE c_bit IS NOT NULL LIMIT 1;
SELECT 'varbit_val', coalesce(c_varbit::text, 'NULL') FROM app.t_all_types WHERE c_varbit IS NOT NULL LIMIT 1;
SELECT 'tid_val', coalesce(c_tid::text, 'NULL') FROM app.t_all_types WHERE c_tid IS NOT NULL LIMIT 1;
SELECT 'xid_val', coalesce(c_xid::text, 'NULL') FROM app.t_all_types WHERE c_xid IS NOT NULL LIMIT 1;
SELECT 'money_val', coalesce(c_money::text, 'NULL') FROM app.t_all_types WHERE c_money IS NOT NULL LIMIT 1;
SELECT 'float4_nan', count(*) FROM app.t_all_types WHERE c_float4='NaN';
SELECT 'float8_inf', count(*) FROM app.t_all_types WHERE c_float8='Infinity';
SELECT 'ts_inf', count(*) FROM app.t_all_types WHERE c_timestamp='infinity';
SELECT 'jsonb_nested', coalesce(c_jsonb::text, 'NULL') FROM app.t_all_types WHERE c_jsonb IS NOT NULL AND c_jsonb::text LIKE '%nested%' LIMIT 1;
SELECT 'jsonb_big', count(*) FROM app.t_all_types WHERE c_jsonb::text = '{"big": "y"}';
SELECT 'toast_len', coalesce(length(c_text)::text, 'NULL') FROM app.t_all_types WHERE c_text LIKE 'TOAST%' LIMIT 1;
SELECT 'arr_int', coalesce(c_arr_int::text, 'NULL') FROM app.t_all_types WHERE c_arr_int IS NOT NULL LIMIT 1;
SELECT 'arr_text', coalesce(c_arr_text::text, 'NULL') FROM app.t_all_types WHERE c_arr_text IS NOT NULL LIMIT 1;
SELECT 'bytea_val', coalesce(c_bytea::text, 'NULL') FROM app.t_all_types WHERE c_bytea IS NOT NULL LIMIT 1;
SELECT 'interval_val', coalesce(c_interval::text, 'NULL') FROM app.t_all_types WHERE c_interval IS NOT NULL LIMIT 1;
SELECT 'macaddr_val', coalesce(c_macaddr::text, 'NULL') FROM app.t_all_types WHERE c_macaddr IS NOT NULL LIMIT 1;
SELECT 'cidr_val', coalesce(c_cidr::text, 'NULL') FROM app.t_all_types WHERE c_cidr IS NOT NULL LIMIT 1;
SELECT 'name_val', coalesce(c_name::text, 'NULL') FROM app.t_all_types WHERE c_name IS NOT NULL LIMIT 1;
SELECT 'bool_val', coalesce(c_bool::text, 'NULL') FROM app.t_all_types WHERE c_bool IS NOT NULL LIMIT 1;
SELECT 'oid_val', coalesce(c_oid::text, 'NULL') FROM app.t_all_types WHERE c_oid IS NOT NULL LIMIT 1;
SELECT 'cid_val', coalesce(c_cid::text, 'NULL') FROM app.t_all_types WHERE c_cid IS NOT NULL LIMIT 1;
EOF

gsql -d appdb -p "$SRC_PORT" -f "$OUT_DIR/compare.sql" 2>&1 | grep -v "total time" > "$OUT_DIR/src_values.txt"
gsql -d import_test -p "$SRC_PORT" -f "$OUT_DIR/compare.sql" 2>&1 | grep -v "total time" > "$OUT_DIR/dst_values.txt"

sort "$OUT_DIR/src_values.txt" > "$OUT_DIR/src_sorted.txt" 2>/dev/null
sort "$OUT_DIR/dst_values.txt" > "$OUT_DIR/dst_sorted.txt" 2>/dev/null

if diff -q "$OUT_DIR/src_sorted.txt" "$OUT_DIR/dst_sorted.txt" >/dev/null 2>&1; then
  pass "逐值比对（全部一致）"
else
  fail "逐值比对（存在差异）"
  log "=== DIFF ==="
  diff "$OUT_DIR/src_sorted.txt" "$OUT_DIR/dst_sorted.txt" | tee -a "$RESULT_FILE"
fi

# CSV 导入比对
log "CSV 导入比对"
gsql -d import_test -p "$SRC_PORT" -c "DROP TABLE IF EXISTS csv_test; CREATE TABLE csv_test (LIKE app.t_all_types INCLUDING ALL);" 2>&1 | tail -1
gsql -d import_test -p "$SRC_PORT" -c "\COPY csv_test FROM '$OUT_DIR/t_all_types.csv' WITH (FORMAT csv, NULL '\\N')" > "$OUT_DIR/csv_import.log" 2>&1
if [ $? -eq 0 ]; then
  csv_cnt=$(gsql -d import_test -p "$SRC_PORT" -t -c "SELECT count(*) FROM csv_test;" 2>/dev/null | tr -d ' ')
  src_cnt=$(gsql -d appdb -p "$SRC_PORT" -t -c "SELECT count(*) FROM app.t_all_types;" 2>/dev/null | tr -d ' ')
  if [ "$csv_cnt" = "$src_cnt" ]; then
    pass "CSV 导入比对 (count=$csv_cnt)"
  else
    fail "CSV 导入比对 (src=$src_cnt, csv=$csv_cnt)"
  fi
else
  fail "CSV 导入失败"
  cat "$OUT_DIR/csv_import.log" | tee -a "$RESULT_FILE"
fi

# ============================================================
# 阶段5: 汇总与清理
# ============================================================
log "===== 版本 $VERSION 测试完成 ====="
if [ "$FAIL_COUNT" -eq 0 ]; then
  log "RESULT: ALL PASS"
  echo "ALL_PASS" > "$OUT_DIR/status.txt"
else
  log "RESULT: $FAIL_COUNT FAILURES"
  echo "FAIL_$FAIL_COUNT" > "$OUT_DIR/status.txt"
fi

# 停止实例释放资源
log "停止实例 (port=$SRC_PORT)"
ps aux | grep "gaussdb.*$PGDATA" | grep -v grep | awk '{print $2}' | xargs -r kill 2>/dev/null
sleep 2
ps aux | grep "gaussdb.*$PGDATA" | grep -v grep | awk '{print $2}' | xargs -r kill -9 2>/dev/null
log "实例已停止"

exit $FAIL_COUNT
