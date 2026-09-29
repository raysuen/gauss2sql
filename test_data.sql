-- ============================================================
-- Author: raysuen
-- gauss2sql 全版本回归测试 - 造数脚本
-- 覆盖：全类型边界、TOAST、分区表、jsonb、tid、已删除行、空表、大行数
-- ============================================================

-- 1. 枚举类型
DROP TYPE IF EXISTS app.mood CASCADE;
CREATE TYPE app.mood AS ENUM ('sad', 'ok', 'happy');

-- 2. 全类型边界表 (33列)
DROP TABLE IF EXISTS app.t_all_types CASCADE;
CREATE TABLE app.t_all_types (
  c_int2 int2,
  c_int4 int4,
  c_int8 int8,
  c_float4 float4,
  c_float8 float8,
  c_numeric numeric(12,4),
  c_text text,
  c_varchar varchar(10000),
  c_bpchar bpchar(20),
  c_bool bool,
  c_bytea bytea,
  c_name name,
  c_date timestamp,
  c_time time,
  c_timestamp timestamp,
  c_timestamptz timestamptz,
  c_interval interval,
  c_uuid uuid,
  c_json json,
  c_jsonb jsonb,
  c_inet inet,
  c_cidr cidr,
  c_macaddr macaddr,
  c_bit bit(8),
  c_varbit varbit(20),
  c_money money,
  c_mood app.mood,
  c_arr_int _int4,
  c_arr_text _text,
  c_oid oid,
  c_tid tid,
  c_xid xid,
  c_cid cid
);

-- 行1: 全类型正常值（含中文特殊字符）
INSERT INTO app.t_all_types VALUES (
  123, -456789, 9223372036854775807,
  3.141590118408203, -25000000000.0, 12345.6789,
  E'中文文本 with ''quote'' and "double" and \\backslash\nnewline\rCR\ttab',
  'varchar中文', 'bpchar中文        ',
  true, E'\\x00ff10', 'name中文',
  '2024-05-06 00:00:00', '23:59:58.123456',
  '2024-05-06 12:34:56.789000', '2024-05-06 04:34:56.789000+00',
  '3 days 04:05:06', '12345678-1234-5678-1234-567812345678',
  '{"a": 1, "b": "中文", "c": [1,2,3]}',
  '{"k": "v", "nested": {"x": [true, null]}}',
  '192.168.1.1/24', '10.0.0.0/8', '08:00:2b:01:02:03',
  '10101010', '10101010101010101010',
  '123.45', 'happy',
  '{1,NULL,3}', '{a,中文,"c,comma"}',
  16388, '(1,2)', '123', '456'
);

-- 行2: 全 NULL
INSERT INTO app.t_all_types VALUES (
  NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,
  NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,
  NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,
  NULL,NULL,NULL
);

-- 行3: TOAST 大字段 + jsonb 第二组
INSERT INTO app.t_all_types (c_text, c_jsonb) VALUES (
  repeat('TOAST大字段测试-中文-', 200),
  '{"big": "y"}'
);

-- 行4: NaN / ±Infinity / ±infinity 时间
INSERT INTO app.t_all_types (c_float4, c_float8, c_timestamp, c_timestamptz, c_text) VALUES (
  'NaN', 'Infinity', 'infinity', '-infinity', E'字面\\N and empty:'
);

-- 行5: 全 NULL（占位）
INSERT INTO app.t_all_types DEFAULT VALUES;

-- 3. TOAST 外联表（大 bytea 触发外部 TOAST）
DROP TABLE IF EXISTS app.t_toast CASCADE;
CREATE TABLE app.t_toast (
  id int4,
  data bytea
);
INSERT INTO app.t_toast VALUES (1, decode(repeat('ab', 16000), 'hex'));
INSERT INTO app.t_toast VALUES (2, decode(repeat('cd', 16000), 'hex'));
INSERT INTO app.t_toast VALUES (3, NULL);

-- 4. 分区表 (RANGE，openGauss 内联分区定义语法)
DROP TABLE IF EXISTS app.t_part CASCADE;
CREATE TABLE app.t_part (
  id int4,
  val text
) PARTITION BY RANGE (id) (
  PARTITION p0 VALUES LESS THAN (100),
  PARTITION p1 VALUES LESS THAN (200)
);
INSERT INTO app.t_part VALUES (1, 'p0-row1');
INSERT INTO app.t_part VALUES (50, 'p0-row2');
INSERT INTO app.t_part VALUES (150, 'p1-row1');

-- 5. jsonb 实验表（4组已知值，用于磁盘格式验证）
DROP TABLE IF EXISTS public.jb_test CASCADE;
CREATE TABLE public.jb_test (
  id int4,
  j jsonb
);
INSERT INTO public.jb_test VALUES (1, '{"k": "v", "nested": {"x": [true, null]}}');
INSERT INTO public.jb_test VALUES (2, '{"a": false}');
INSERT INTO public.jb_test VALUES (3, '[true, false]');
INSERT INTO public.jb_test VALUES (4, '123.5');
INSERT INTO public.jb_test VALUES (5, '[1, "two", null]');
INSERT INTO public.jb_test VALUES (6, '{"big": "y"}');

-- 6. tid 测试表
DROP TABLE IF EXISTS public.t_tid CASCADE;
CREATE TABLE public.t_tid (
  id int4,
  t tid
);
INSERT INTO public.t_tid VALUES (1, '(1,2)');
INSERT INTO public.t_tid VALUES (2, '(65536,2)');
INSERT INTO public.t_tid VALUES (3, '(0,1)');

-- 7. 空表
DROP TABLE IF EXISTS public.t_empty CASCADE;
CREATE TABLE public.t_empty (
  id int4,
  val text
);

-- 8. 简单表（用于已删除行测试 + >100行 parallel 测试）
DROP TABLE IF EXISTS app.t_simple CASCADE;
CREATE TABLE app.t_simple (
  id int4,
  name text,
  score numeric(10,2)
);

-- 插入 150 行（用于 --parallel 测试）
INSERT INTO app.t_simple (id, name, score)
SELECT g, 'row-' || g, (g * 1.5)::numeric(10,2)
FROM generate_series(1, 150) g;

-- 删除一些行（留 t_xmax）
DELETE FROM app.t_simple WHERE id IN (5, 10, 15, 20, 25);
-- 更新一些行（旧版本留 t_xmax）
UPDATE app.t_simple SET name = 'updated-' || id WHERE id IN (30, 31, 32);

-- 9. CHECKPOINT 强制落盘
CHECKPOINT;
