// gauss2sql-go 命令行入口（Author: raysuen）
// 移植自 gauss2sql/main.py：离线解析 openGauss 堆文件，生成 DDL/SQL/CSV/meta。
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gauss2sql-go/internal/binary"
	"gauss2sql-go/internal/catalog"
	"gauss2sql-go/internal/heapfile"
	"gauss2sql-go/internal/meta"
	"gauss2sql-go/internal/toast"
	"gauss2sql-go/internal/types"
)

var version = "0.2.20"

type options struct {
	dataPath      string
	datadir       string
	dbId          string
	listDB        bool
	listTablesDB  bool
	listTablesAll bool
	exportMeta    bool
	tables        bool
	allTables     bool
	schema        string
	ddl           bool
	sql           bool
	data          bool
	output        string
	parallel      int
	limit         int
	delimiter     string
	deleted       bool
	onlyDeleted   bool
	count         bool
	fields        string
	header        bool
	encoding      string
	catalogJSON   string
	tableName     string
	showVersion   bool
	verbose       bool
	batchRows     int    // v0.2.20: 每 N 行合并为一条多值 INSERT；0=关闭（默认，逐字节兼容）
	jobs          int    // v0.2.20: 批量导出表间并行 worker 数；1=串行（默认，逐字节兼容）
	report        string // v0.2.20: 导出报告 JSON 路径；空=关闭
}

func parseArgs() *options {
	o := &options{}
	var v string
	args := os.Args[1:]
	// argVal：安全取选项值；缺参数（选项在末尾）时报错退出而非越界 panic
	argVal := func(args []string, i int, name string) (string, int) {
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "error: 选项 %s 缺少参数值\n", name)
			os.Exit(2)
		}
		return args[i+1], i + 1
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--list-db":
			o.listDB = true
		case "--list-tables-db":
			o.listTablesDB = true
		case "--list-tables-all":
			o.listTablesAll = true
		case "--export-meta":
			o.exportMeta = true
		case "--tables":
			o.tables = true
		case "--all-tables":
			o.allTables = true
		case "--schema":
			o.schema, i = argVal(args, i, a)
		case "--ddl":
			o.ddl = true
		case "--sql":
			o.sql = true
		case "--data":
			o.data = true
		case "--deleted":
			o.deleted = true
		case "--only-deleted":
			o.onlyDeleted = true
		case "--count":
			o.count = true
		case "--header":
			o.header = true
		case "--help", "-h":
			printHelp()
			os.Exit(0)
		case "--verbose":
			o.verbose = true
		case "--version":
			o.showVersion = true
		case "--datadir":
			o.datadir, i = argVal(args, i, a)
		case "--db-id":
			o.dbId, i = argVal(args, i, a)
		case "--output", "-o":
			o.output, i = argVal(args, i, a)
		case "--parallel", "-j":
			v, i = argVal(args, i, a)
			o.parallel, _ = strconv.Atoi(v)
		case "--limit":
			v, i = argVal(args, i, a)
			o.limit, _ = strconv.Atoi(v)
		case "--delimiter":
			o.delimiter, i = argVal(args, i, a)
		case "--fields":
			o.fields, i = argVal(args, i, a)
		case "--encoding":
			o.encoding, i = argVal(args, i, a)
		case "--catalog-json":
			o.catalogJSON, i = argVal(args, i, a)
		case "--table-name":
			o.tableName, i = argVal(args, i, a)
		case "--batch-rows":
			v, i = argVal(args, i, a)
			o.batchRows, _ = strconv.Atoi(v)
		case "--jobs":
			v, i = argVal(args, i, a)
			o.jobs, _ = strconv.Atoi(v)
		case "--report":
			o.report, i = argVal(args, i, a)
		default:
			if !strings.HasPrefix(a, "-") && o.dataPath == "" {
				o.dataPath = a
			}
		}
	}
	return o
}

func main() {
	o := parseArgs()
	if o.delimiter == "" {
		o.delimiter = "," // --delimiter 默认逗号（与 COPY DELIMITER 语义一致）
	}
	if o.showVersion {
		fmt.Printf("gauss2sql-go %s (Author: raysuen)\n", version)
		return
	}
	// 定位目标数据路径：优先位置参数；否则 --datadir + --db-id 组合
	// （v0.2.16）可驱动 --list-tables-db/--table-name/--schema/--tables 等导出模式。
	// 位置参数与 --db-id 并存、向后兼容：老命令 gauss2sql base/16388 --list-tables-db 行为不变。
	resolveDataPath := func() (string, error) {
		if o.dataPath != "" {
			abs, _ := filepath.Abs(o.dataPath)
			return abs, nil
		}
		if o.datadir != "" && o.dbId != "" {
			abs := filepath.Join(o.datadir, "base", o.dbId)
			abs, _ = filepath.Abs(abs)
			return abs, nil
		}
		return "", fmt.Errorf("usage: gauss2sql-go <data-file-or-db-dir> [options]  (或用 --datadir DIR --db-id OID)")
	}

	// --list-db 可仅用 --datadir；其余模式需要位置参数 dataPath 或 --datadir+--db-id
	if o.dataPath == "" && !o.listDB && !(o.datadir != "" && o.dbId != "") {
		fmt.Fprintln(os.Stderr, "usage: gauss2sql-go <data-file-or-db-dir> [options]  (或用 --datadir DIR --db-id OID)")
		os.Exit(2)
	}
	if o.listDB && o.dataPath == "" && o.datadir == "" {
		fmt.Fprintln(os.Stderr, "--list-db 需要数据目录路径")
		os.Exit(2)
	}

	if o.listDB {
		listDBs(o)
		return
	}

	abs, err := resolveDataPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := os.Stat(abs); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if o.listTablesDB {
		listTablesDB(abs, false)
		return
	}
	if o.listTablesAll {
		listTablesDB(abs, true)
		return
	}
	if o.exportMeta {
		exportMeta(abs, o)
		return
	}
	// --table-name（无 --catalog-json）+ 数据库目录：按表名直查导出（v0.2.12）
	if o.tableName != "" && o.catalogJSON == "" {
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			exportOneTableByName(abs, o)
			return
		}
		// 位置参数为单个数据文件时忽略 --table-name（走自动发现，保持原行为）
	}
	if o.tables || o.allTables || o.schema != "" {
		exportDBAll(abs, o)
		return
	}

	// 单文件模式
	dbDir := filepath.Dir(abs)
	// 探测编码
	dbOid, _ := strconv.Atoi(filepath.Base(dbDir))
	if enc := catalog.DetectDatabaseEncoding(filepath.Dir(filepath.Dir(dbDir)), dbOid); enc != "" {
		binary.SetTextEncoding(enc)
	}
	if o.encoding != "" {
		binary.SetTextEncoding(o.encoding)
	}
	catalog.LoadEnumMap(dbDir)
	tm := resolveTableMeta(abs, o)
	if o.count {
		n := countTable(abs, o, tm)
		fmt.Printf("-- 总行数: %d\n", n)
		if o.verbose {
			vlog(o, "统计完成: %d 行", n)
		}
		return
	}
	ext := "sql"
	if o.data {
		ext = "csv"
	}
	start := time.Now()
	var teeW *teeWriter
	var w *bufio.Writer
	var done func()
	var outPath string
	if o.report != "" && o.output == "" {
		// stdout + --report：用 tee writer 同时累计 bytes/sha256
		teeW = newTeeWriter(os.Stdout)
		w = bufio.NewWriterSize(teeW, 1<<20)
		done = func() { _ = w.Flush() }
	} else {
		w, done, outPath = openOutWriter(o, tm, ext)
	}
	nRows, _, corrupt := streamTable(abs, o, w)
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
	finishSingleTableReport(o, dbOid, tm, outPath, nRows, corrupt, start, teeW)
}

// openOutWriter 打开输出 writer：-o 未指定 → stdout（不关闭）；否则按 resolveOutput 规则创建文件。
// 返回 writer、完成回调（flush+close）、输出路径（""=标准输出）。
func openOutWriter(o *options, tm *meta.TableMeta, ext string) (*bufio.Writer, func(), string) {
	if o.output == "" {
		w := bufio.NewWriterSize(os.Stdout, 1<<20)
		return w, func() { _ = w.Flush() }, ""
	}
	path := resolveOutput(o.output, tm, ext)
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	return w, func() { _ = w.Flush(); _ = f.Close() }, path
}

// verboseReport 输出 --verbose 报告（模式/输出目标/行数/耗时）
func verboseReport(o *options, tm *meta.TableMeta, outPath string, nRows int, ext string, start time.Time) {
	if !o.verbose {
		return
	}
	mode := "sql"
	if ext == "csv" {
		mode = "csv"
	}
	if o.ddl && !o.data {
		mode = "ddl+sql"
	}
	vlog(o, "模式: %s (parallel=%d, encoding=%s)", mode, o.parallel, binary.GetTextEncoding())
	if outPath == "" {
		vlog(o, "输出到: 标准输出")
	} else {
		vlog(o, "输出到: %s", outPath)
	}
	vlog(o, "完成: %d 行, 耗时 %.2fs", nRows, time.Since(start).Seconds())
}

// exportOneTableByName 位置参数为数据库目录 + --table-name（无 --catalog-json）时按表名直查导出（v0.2.12）。
// 支持 "schema.table" 与裸表名（后者要求全库唯一）；分区父表展开全部子分区文件导出。
func exportOneTableByName(dbDir string, o *options) {
	dbOid, _ := strconv.Atoi(filepath.Base(dbDir))
	if enc := catalog.DetectDatabaseEncoding(filepath.Dir(filepath.Dir(dbDir)), dbOid); enc != "" {
		binary.SetTextEncoding(enc)
	}
	if o.encoding != "" {
		binary.SetTextEncoding(o.encoding)
	}
	catalog.LoadEnumMap(dbDir)

	tm, filePath, err := catalog.FindTableMetaByName(dbDir, o.tableName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	vlog(o, "按表名定位: %s.%s (relfilenode=%d, relkind=%s)", tm.Schema, tm.Relname, tm.Relfilenode, tm.Relkind)

	if tm.Relfilenode == 0 {
		// 分区父表：DDL 一次 + 全部子分区数据
		exportPartitionParent(dbDir, tm, o)
		return
	}
	if filePath == "" {
		fmt.Fprintf(os.Stderr, "error: 表 %s 无物理数据文件\n", o.tableName)
		os.Exit(1)
	}
	if o.count {
		n := countTable(filePath, o, tm)
		fmt.Printf("-- 总行数: %d\n", n)
		if o.verbose {
			vlog(o, "统计完成: %d 行", n)
		}
		return
	}
	ext := "sql"
	if o.data {
		ext = "csv"
	}
	start := time.Now()
	var teeW *teeWriter
	var w *bufio.Writer
	var done func()
	var outPath string
	if o.report != "" && o.output == "" {
		teeW = newTeeWriter(os.Stdout)
		w = bufio.NewWriterSize(teeW, 1<<20)
		done = func() { _ = w.Flush() }
	} else {
		w, done, outPath = openOutWriter(o, tm, ext)
	}
	nRows, corrupt := streamKnownTable(filePath, dbDir, tm, o, w)
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
	finishSingleTableReport(o, dbOid, tm, outPath, nRows, corrupt, start, teeW)
}

// exportPartitionParent 分区父表导出（v0.2.13 流式）：DDL 使用父表列定义输出一次，
// 数据按全部子分区文件逐个流式解析并输出（行顺序 = pg_partition 扫描顺序），最后统一追加 setval。
// v0.2.18：核心计数/写盘逻辑抽为 countPartitionParent / exportPartitionParentToWriter，
// 供批量导出 exportDBAll 复用，单表行为与 v0.2.17 完全一致。
func exportPartitionParent(dbDir string, tm *meta.TableMeta, o *options) {
	files := catalog.ListPartitionFiles(dbDir, tm.Reloid, tm.Reltablespace)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: 分区父表 %s 无子分区数据文件\n", o.tableName)
		os.Exit(1)
	}
	vlog(o, "分区父表: 展开 %d 个子分区文件", len(files))

	validateFields(tm, parseFields(o.fields))

	if o.count {
		total := countPartitionParent(dbDir, tm, o)
		fmt.Printf("-- 总行数: %d\n", total)
		return
	}

	ext := "sql"
	if o.data {
		ext = "csv"
	}
	dbOid, _ := strconv.Atoi(filepath.Base(dbDir))
	start := time.Now()
	var teeW *teeWriter
	var w *bufio.Writer
	var done func()
	var outPath string
	if o.report != "" && o.output == "" {
		teeW = newTeeWriter(os.Stdout)
		w = bufio.NewWriterSize(teeW, 1<<20)
		done = func() { _ = w.Flush() }
	} else {
		w, done, outPath = openOutWriter(o, tm, ext)
	}
	nRows, corrupt := exportPartitionParentToWriter(dbDir, tm, o, w)
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
	finishSingleTableReport(o, dbOid, tm, outPath, nRows, corrupt, start, teeW)
}

// countPartitionParent 统计分区父表总行数 = 各子分区行数之和（v0.2.18 抽出）。
// 行为与 v0.2.17 单表 count 分支逐行一致：不强制挂载父表 TOAST（计数不需要解压 TOAST）。
func countPartitionParent(dbDir string, tm *meta.TableMeta, o *options) int {
	files := catalog.ListPartitionFiles(dbDir, tm.Reloid, tm.Reltablespace)
	total := 0
	for _, f := range files {
		sub := catalog.AutoDiscoverMetaInDir(f, dbDir, 0)
		if sub == nil {
			continue
		}
		hf := subHeapFile(f, o, dbDir, tm, sub)
		n, _ := hf.StreamRowsCount(sub, o.deleted, o.onlyDeleted, o.limit)
		total += n
	}
	return total
}

// exportPartitionParentToWriter 把分区父表展开写入指定 writer（DDL 一次 + 逐子分区数据 + setval），
// 返回全部子分区行数之和与坏页数之和。v0.2.18 抽出：批量 exportDBAll 与单表
// exportPartitionParent 共用，输出文件由调用方决定（单表=openOutWriter；批量=-o 目录下 schema.父表名.ext）。
// v0.2.20：SQL 模式下所有子分区共享同一个 batchSink（同一张逻辑表，列名一致可跨子分区分批合并）。
func exportPartitionParentToWriter(dbDir string, tm *meta.TableMeta, o *options, w io.Writer) (int, int) {
	files := catalog.ListPartitionFiles(dbDir, tm.Reloid, tm.Reltablespace)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: 分区父表 %s.%s 无子分区数据文件\n", tm.Schema, tm.Relname)
		os.Exit(1)
	}
	vlog(o, "分区父表: 展开 %d 个子分区文件", len(files))

	fields := parseFields(o.fields)
	validateFields(tm, fields)

	var nRows, totalCorrupt int
	if o.ddl {
		fmt.Fprintf(w, "%s\n\n", buildDDL(tm, dbDir))
	}
	// SQL 模式：创建一次 batch sink，跨所有子分区复用（同一张逻辑表）
	var outCb func(string) error
	var flush func() error
	if o.sql {
		outCb, flush = makeBatchWriter(w, o.batchRows)
	}
	for _, f := range files {
		sub := catalog.AutoDiscoverMetaInDir(f, dbDir, 0)
		if sub == nil {
			fmt.Fprintf(os.Stderr, "warning: 子分区文件无法解析结构, 跳过: %s\n", f)
			continue
		}
		if sub.Toastrelid == 0 {
			sub.Toastrelid = tm.Toastrelid // 分区表 TOAST 在父表上，子分区继承
		}
		hf := subHeapFile(f, o, dbDir, tm, sub)
		if o.sql {
			n, err := hf.StreamSQL(sub, o.deleted, o.onlyDeleted, o.limit, true, false, fields, outCb)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			nRows += n
			totalCorrupt += hf.CorruptPages
		}
		if o.data {
			n, err := hf.StreamToData(sub, o.deleted, o.onlyDeleted, o.limit, o.delimiter, fields, o.header, writeLine(w))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			nRows += n
		}
	}
	if o.sql {
		_ = flush()
		for _, s := range buildSetvalStmts(tm, dbDir) {
			fmt.Fprintln(w, s)
		}
	}
	return nRows, totalCorrupt
}

// subHeapFile 创建子分区 HeapFile（TOAST 继承父表后挂载）
func subHeapFile(f string, o *options, dbDir string, tm *meta.TableMeta, sub *meta.TableMeta) *heapfile.HeapFile {
	hf := heapfile.NewHeapFile(f, 0)
	if o.parallel > 1 {
		hf.Parallel = o.parallel
	}
	if sub.Toastrelid != 0 {
		tp := catalog.FindPhysicalFileByOid(dbDir, sub.Toastrelid)
		if tp == "" {
			tp = filepath.Join(dbDir, strconv.Itoa(sub.Toastrelid))
		}
		if _, err := os.Stat(tp); err == nil {
			tt := toast.New(tp, 0)
			tt.BuildIndex()
			hf.Toast = tt
		}
	}
	return hf
}

// resolveTableMeta 解析表元数据（--catalog-json 优先，否则自动发现）；失败时退出。
func resolveTableMeta(abs string, o *options) *meta.TableMeta {
	var tm *meta.TableMeta
	if o.catalogJSON != "" {
		tm = loadCatalogJSON(o.catalogJSON, o.tableName, filepath.Base(abs))
		if tm != nil {
			vlog(o, "从 JSON 加载元数据: %s.%s (%d 列)", tm.Schema, tm.Relname, len(tm.Columns))
		}
	}
	if tm == nil {
		tm = catalog.AutoDiscoverMeta(abs, 0)
		if tm != nil {
			vlog(o, "自动发现表结构: %s.%s (%d 列, relfilenode=%d)", tm.Schema, tm.Relname, len(tm.Columns), tm.Relfilenode)
		}
	}
	if tm == nil {
		fmt.Fprintln(os.Stderr, "error: cannot discover table structure")
		os.Exit(1)
	}
	return tm
}

// newHeapFile 创建堆文件读取器并挂载 TOAST（light 索引）
func newHeapFile(abs string, o *options, dbDir string, tm *meta.TableMeta) *heapfile.HeapFile {
	hf := heapfile.NewHeapFile(abs, 0)
	if o.parallel > 1 {
		hf.Parallel = o.parallel
	}
	if tm.Toastrelid != 0 {
		tp := catalog.FindPhysicalFileByOid(dbDir, tm.Toastrelid)
		if tp == "" {
			tp = filepath.Join(dbDir, strconv.Itoa(tm.Toastrelid))
		}
		if _, err := os.Stat(tp); err == nil {
			tt := toast.New(tp, 0)
			tt.BuildIndex()
			hf.Toast = tt
			vlog(o, "TOAST: %s (light 索引)", tp)
		}
	}
	return hf
}

// validateFields 校验 --fields 字段存在性；缺失时退出
func validateFields(tm *meta.TableMeta, fields map[string]bool) {
	if len(fields) == 0 {
		return
	}
	valid := map[string]bool{}
	for _, c := range tm.Columns {
		if !c.Attdropped {
			valid[c.Name] = true
		}
	}
	var missing []string
	for f := range fields {
		if !valid[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		fmt.Fprintln(os.Stderr, "--fields 中不存在的字段:", strings.Join(missing, ","))
		os.Exit(1)
	}
}

// writeLine 行写入回调（适配流式 API）
func writeLine(w io.Writer) func(string) error {
	return func(line string) error {
		_, err := io.WriteString(w, line)
		return err
	}
}

// batchSink 把 StreamSQL 逐行吐出的单行 INSERT 合并为多值 INSERT（v0.2.20 --batch-rows）。
// 输入行格式（heapfile.StreamSQL 产生）：
//
//	普通行: "INSERT INTO \"s\".\"t\" (cols) VALUES (body);\n"
//	删除行: "-- DELETED ctid=X\nINSERT INTO \"s\".\"t\" (cols) VALUES (body);\n"
//
// 合并输出（每 batchSize 行一条）：
//
//	INSERT INTO "s"."t" (cols) VALUES
//	(row1),
//	(row2),
//	...
//	(rowN);
//
// 删除行附带 -- DELETED 注释，无法嵌入多值括号列表，因此遇到删除行先 flush 当前批次，
// 再将该删除行作为独立单行 INSERT 输出（注释保留），不破坏正确性。
type batchSink struct {
	w         io.Writer
	batchSize int
	count     int // 当前已攒入未闭合批次的行数
}

// makeBatchWriter 包装底层 writer 为 StreamSQL 的 out 回调；返回回调与 flush 函数。
// batchRows<=0 时回调与 writeLine(w) 完全等价（逐字节一致），flush 为 no-op。
// 仅在 SQL 模式下使用；CSV/--data 模式不经过此 sink。
func makeBatchWriter(w io.Writer, batchRows int) (func(string) error, func() error) {
	if batchRows <= 0 {
		return writeLine(w), func() error { return nil }
	}
	b := &batchSink{w: w, batchSize: batchRows}
	return b.write, b.flush
}

func (b *batchSink) write(line string) error {
	// 剥离可选的 "-- DELETED ctid=...\n" 前缀
	comment := ""
	rest := line
	if strings.HasPrefix(rest, "-- DELETED ") {
		if nl := strings.Index(rest, "\n"); nl >= 0 {
			comment = rest[:nl+1]
			rest = rest[nl+1:]
		}
	}
	// rest 形如: INSERT INTO "s"."t" (cols) VALUES (body);\n
	rest = strings.TrimSuffix(rest, "\n")
	rest = strings.TrimSuffix(rest, ";")
	// 定位 " VALUES " 关键字（列名列表不含该关键字；值串中若出现 " VALUES ("
	// 也在真正的 VALUES 之后，故取首个出现位置即可安全切分）
	at := strings.Index(rest, " VALUES ")
	if at < 0 {
		// 非预期格式：闭合当前批次后原样透传，绝不吞字节
		_ = b.flush()
		_, err := io.WriteString(b.w, line)
		return err
	}
	prefix := rest[:at] + " VALUES"     // INSERT INTO "s"."t" (cols) VALUES
	body := rest[at+len(" VALUES "):]  // (row values)

	// 删除行：先闭合批次，再作为独立单行 INSERT 输出（保留 -- DELETED 注释）
	if comment != "" {
		_ = b.flush()
		if _, err := io.WriteString(b.w, comment); err != nil {
			return err
		}
		_, err := io.WriteString(b.w, prefix+"\n"+body+";\n")
		return err
	}

	if b.count == 0 {
		// 批次首行：写 "PREFIX\n(row1)"
		if _, err := io.WriteString(b.w, prefix+"\n"+body); err != nil {
			return err
		}
		b.count = 1
	} else {
		// 批次后续行：写 ",\n(rowN)"
		if _, err := io.WriteString(b.w, ",\n"+body); err != nil {
			return err
		}
		b.count++
	}
	if b.count >= b.batchSize {
		// 攒满 N 行：收尾 ";\n"
		if _, err := io.WriteString(b.w, ";\n"); err != nil {
			return err
		}
		b.count = 0
	}
	return nil
}

// flush 在流结束 / --limit 截断 / 切换表时调用，把未攒满的批次用分号闭合。
func (b *batchSink) flush() error {
	if b.count > 0 {
		b.count = 0
		_, err := io.WriteString(b.w, ";\n")
		return err
	}
	return nil
}

// teeWriter 同时写入底层 writer 并累计 SHA-256/字节数（v0.2.20 --report stdout 模式）。
type teeWriter struct {
	w      io.Writer
	hasher hash.Hash
	n      int64
}

func newTeeWriter(w io.Writer) *teeWriter {
	return &teeWriter{w: w, hasher: sha256.New()}
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.hasher.Write(p[:n])
		t.n += int64(n)
	}
	return n, err
}

// finishSingleTableReport 单表导出结束后收集记录并写 JSON 报告（v0.2.20）。
// teeW 非 nil 表示 stdout 模式（用 tee 累计 bytes/sha256）；outPath 非空表示文件模式（stat+读回）。
func finishSingleTableReport(o *options, dbOid int, tm *meta.TableMeta, outPath string, nRows, corrupt int, start time.Time, teeW *teeWriter) {
	if o.report == "" {
		return
	}
	rec := tableReportRecord{
		Schema: tm.Schema, Name: tm.Relname, Oid: tm.Reloid,
		Rows: nRows, CorruptPages: corrupt,
		DurationMs: time.Since(start).Milliseconds(),
	}
	if teeW != nil {
		rec.Bytes = teeW.n
		rec.SHA256 = hex.EncodeToString(teeW.hasher.Sum(nil))
	} else if outPath != "" {
		if fi, err := os.Stat(outPath); err == nil {
			rec.Bytes = fi.Size()
		}
		if data, err := os.ReadFile(outPath); err == nil {
			sum := sha256.Sum256(data)
			rec.SHA256 = hex.EncodeToString(sum[:])
		}
	}
	writeReportJSON(o, o.report, dbOid, []tableReportRecord{rec})
}

// streamTable 流式导出单表（DDL/SQL/CSV/setval 直接写入 w，v0.2.13 A+B：内存 O(块)）。
// 返回行数、元数据、坏页数；行数语义与旧版一致（data 覆盖 sql）。
func streamTable(abs string, o *options, w io.Writer) (int, *meta.TableMeta, int) {
	dbDir := filepath.Dir(abs)
	tm := resolveTableMeta(abs, o)
	hf := newHeapFile(abs, o, dbDir, tm)
	fields := parseFields(o.fields)
	validateFields(tm, fields)

	var nRows int
	if o.ddl {
		fmt.Fprintf(w, "%s\n\n", buildDDL(tm, dbDir))
	}
	if o.sql {
		outCb, flush := makeBatchWriter(w, o.batchRows)
		n, err := hf.StreamSQL(tm, o.deleted, o.onlyDeleted, o.limit, true, false, fields, outCb)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_ = flush()
		nRows = n
		for _, s := range buildSetvalStmts(tm, dbDir) {
			fmt.Fprintln(w, s)
		}
	}
	if o.data {
		n, err := hf.StreamToData(tm, o.deleted, o.onlyDeleted, o.limit, o.delimiter, fields, o.header, writeLine(w))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		nRows = n
	}
	return nRows, tm, hf.CorruptPages
}

// streamKnownTable 流式导出已定位的单表（v0.2.16）：与 streamTable 相同，但复用调用方
// 已发现的 tm 与 catalogDir（系统目录所在库目录 = base/<dbOid>），不再从数据文件路径反推
// dbDir。这样当数据文件落在独立表空间（pg_tblspc/.../<dbOid>/<rfn）时，DDL/setval/TOAST
// 等系统目录查找仍走 base/<dbOid>，而 TOAST 挂载由 FindPhysicalFileByOid 按 reltablespace
// 经 pg_tblspc 解析到表空间内的 TOAST 文件。
func streamKnownTable(absPath string, catalogDir string, tm *meta.TableMeta, o *options, w io.Writer) (int, int) {
	hf := newHeapFile(absPath, o, catalogDir, tm)
	fields := parseFields(o.fields)
	validateFields(tm, fields)

	var nRows int
	if o.ddl {
		fmt.Fprintf(w, "%s\n\n", buildDDL(tm, catalogDir))
	}
	if o.sql {
		outCb, flush := makeBatchWriter(w, o.batchRows)
		n, err := hf.StreamSQL(tm, o.deleted, o.onlyDeleted, o.limit, true, false, fields, outCb)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_ = flush()
		nRows = n
		for _, s := range buildSetvalStmts(tm, catalogDir) {
			fmt.Fprintln(w, s)
		}
	}
	if o.data {
		n, err := hf.StreamToData(tm, o.deleted, o.onlyDeleted, o.limit, o.delimiter, fields, o.header, writeLine(w))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		nRows = n
	}
	return nRows, hf.CorruptPages
}

// countTable 流式统计行数（不写文件）
func countTable(abs string, o *options, tm *meta.TableMeta) int {
	hf := newHeapFile(abs, o, filepath.Dir(abs), tm)
	n, err := hf.StreamRowsCount(tm, o.deleted, o.onlyDeleted, o.limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return n
}

// tableReportRecord 一条导出报告记录（v0.2.20 --report）
type tableReportRecord struct {
	Schema       string `json:"schema"`
	Name         string `json:"name"`
	Oid          int    `json:"oid"`
	Rows         int    `json:"rows"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
	DurationMs   int64  `json:"duration_ms"`
	CorruptPages int   `json:"corrupt_pages"`
}

// exportDBAll 批量导出数据库（或 --schema 过滤的 schema）下所有普通表（--tables/--all-tables）。
// 位置参数为数据库目录（如 base/16388）；必须指定 -o 输出目录且 --sql/--data/--ddl 之一；
// 每表一个 <schema>.<对象名>.<sql|csv|ddl> 文件；--all-tables 时包含系统 schema。
// v0.2.20：--jobs M 表间并行（默认 1 串行，逐字节兼容）；--report PATH 导出 JSON 报告；
// 成功后自动在 -o 目录生成 import.sh。
func exportDBAll(dbDir string, o *options) {
	if !o.sql && !o.data && !o.ddl {
		fmt.Fprintln(os.Stderr, "--tables/--all-tables/--schema 批量导出必须指定 --sql 或 --data（纯 DDL 用 --ddl）")
		os.Exit(2)
	}
	if o.output == "" {
		fmt.Fprintln(os.Stderr, "--tables/--all-tables/--schema 批量导出必须指定输出目录（-o 目录）")
		os.Exit(2)
	}
	if !o.count {
		if fi, err := os.Stat(o.output); err != nil || !fi.IsDir() {
			fmt.Fprintln(os.Stderr, "-o 必须是已存在的目录:", o.output)
			os.Exit(2)
		}
	}
	dbOid, _ := strconv.Atoi(filepath.Base(dbDir))
	if enc := catalog.DetectDatabaseEncoding(filepath.Dir(filepath.Dir(dbDir)), dbOid); enc != "" {
		binary.SetTextEncoding(enc)
	}
	if o.encoding != "" {
		binary.SetTextEncoding(o.encoding)
	}
	catalog.LoadEnumMap(dbDir)

	tables := catalog.AutoDiscoverAllTablesOrdered(dbDir, 0)
	schemaSet := map[string]bool{}
	if o.schema != "" {
		for _, s := range strings.Split(o.schema, ",") {
			if s = strings.TrimSpace(s); s != "" {
				schemaSet[s] = true
			}
		}
	}
	var list []*meta.TableMeta
	for _, tm := range tables {
		// v0.2.18：普通表(r)与分区父表(p)都导出；父表在批量路径展开全部子分区。
		// 仍跳过索引/序列/TOAST/视图等其他 relkind。
		if tm.Relkind != "r" && tm.Relkind != "p" {
			continue
		}
		if len(schemaSet) > 0 {
			if !schemaSet[tm.Schema] {
				continue
			}
		} else if !o.allTables && isSystemSchema(tm.Schema) { // --all-tables 才包含系统 schema
			continue
		}
		list = append(list, tm)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "error: 未发现可导出的表（指定 --schema 或检查数据库目录）")
		os.Exit(1)
	}
	// 确定性输出顺序
	sort.Slice(list, func(i, j int) bool { return list[i].FullName() < list[j].FullName() })

	ext := "sql"
	if o.data {
		ext = "csv"
	} else if o.ddl && !o.sql {
		ext = "ddl"
	}

	start := time.Now()
	// v0.2.18：openGauss 传统分区父表在 pg_class 中 relkind='r'（非 'p'），需经 pg_partition
	// 预先算出"拥有子分区的父表 OID 集合"，批量路径据此展开父表（relkind='p' 的 PG 风格兼容保留）。
	parentOids := catalog.PartitionParentOids(dbDir)

	// --count 模式：只打印行数，不产生文件字节。始终串行，print 顺序与 v0.2.19 一致。
	if o.count {
		total := 0
		for _, tm := range list {
			if parentOids[tm.Reloid] || tm.Relkind == "p" {
				n := countPartitionParent(dbDir, tm, o)
				fmt.Printf("-- %s.%s: %d 行\n", tm.Schema, tm.Relname, n)
				total += n
				continue
			}
			abs := catalog.ResolvePhysicalFile(dbDir, tm.Reltablespace, tm.Relfilenode)
			if abs == "" {
				fmt.Fprintf(os.Stderr, "warning: 跳过 %s.%s（数据文件不存在: %s/%d）\n", tm.Schema, tm.Relname, dbDir, tm.Relfilenode)
				continue
			}
			n := countTable(abs, o, tm)
			fmt.Printf("-- %s.%s: %d 行\n", tm.Schema, tm.Relname, n)
			total += n
		}
		if o.verbose {
			vlog(o, "批量统计完成: %d 张表, %d 行, 耗时 %.2fs", len(list), total, time.Since(start).Seconds())
		}
		return
	}

	// 非 count 模式：写文件。每表导出逻辑抽为 exportOneTableFile，串行/并行共用。
	var records []tableReportRecord
	var recMu sync.Mutex

	exportOne := func(tm *meta.TableMeta) {
		rec := exportOneTableFile(dbDir, tm, o, ext, parentOids)
		if rec.Rows >= 0 { // 未跳过的表才记入报告
			if o.report != "" {
				recMu.Lock()
				records = append(records, rec)
				recMu.Unlock()
			}
			vlog(o, "已导出: %s.%s (%d 行)", rec.Schema, rec.Name, rec.Rows)
		}
	}

	if o.jobs > 1 {
		// v0.2.20：表间并行 worker pool。带缓冲 channel 做 semaphore（容量=jobs）。
		// 系统目录加载已在主线程完成，worker 内只读 catalog 数据，安全。
		sem := make(chan struct{}, o.jobs)
		var wg sync.WaitGroup
		for _, tm := range list {
			tm := tm // capture loop variable
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				exportOne(tm)
			}()
		}
		wg.Wait()
	} else {
		// jobs<=1：串行，表顺序与 v0.2.19 逐字节一致
		for _, tm := range list {
			exportOne(tm)
		}
	}

	total := 0
	for _, r := range records {
		total += r.Rows
	}
	if o.verbose {
		vlog(o, "批量导出完成: %d 张表, %d 行, 耗时 %.2fs", len(list), total, time.Since(start).Seconds())
	}

	// v0.2.20：写导出报告 JSON
	if o.report != "" {
		writeReportJSON(o, o.report, dbOid, records)
	}
	// v0.2.20：批量导出成功后自动生成 import.sh
	if !o.count {
		generateImportSh(o, o.output, ext)
	}
}

// exportOneTableFile 导出单表到 -o 目录下的文件，返回报告记录。
// 物理文件缺失时返回 Rows=-1 表示跳过（不记入报告）。
// 串行（jobs=1）与并行（jobs>1）共用此函数，保证输出文件字节一致。
func exportOneTableFile(dbDir string, tm *meta.TableMeta, o *options, ext string, parentOids map[int]bool) tableReportRecord {
	rec := tableReportRecord{Schema: tm.Schema, Name: tm.Relname, Oid: tm.Reloid}
	start := time.Now()

	var nRows, corrupt int
	path := filepath.Join(o.output, defaultOutName(tm, ext))

	if parentOids[tm.Reloid] || tm.Relkind == "p" {
		// 分区父表：展开全部子分区
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		w := bufio.NewWriterSize(f, 1<<20)
		nRows, corrupt = exportPartitionParentToWriter(dbDir, tm, o, w)
		if err := w.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_ = f.Close()
	} else {
		// 普通表：表空间感知定位
		abs := catalog.ResolvePhysicalFile(dbDir, tm.Reltablespace, tm.Relfilenode)
		if abs == "" {
			fmt.Fprintf(os.Stderr, "warning: 跳过 %s.%s（数据文件不存在: %s/%d）\n", tm.Schema, tm.Relname, dbDir, tm.Relfilenode)
			rec.Rows = -1
			return rec
		}
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		w := bufio.NewWriterSize(f, 1<<20)
		nRows, corrupt = streamKnownTable(abs, dbDir, tm, o, w)
		if err := w.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_ = f.Close()
	}

	rec.Rows = nRows
	rec.CorruptPages = corrupt
	rec.DurationMs = time.Since(start).Milliseconds()
	// 对写出文件本身算字节数和 SHA-256
	if fi, err := os.Stat(path); err == nil {
		rec.Bytes = fi.Size()
	}
	if data, err := os.ReadFile(path); err == nil {
		sum := sha256.Sum256(data)
		rec.SHA256 = hex.EncodeToString(sum[:])
	}
	return rec
}

// writeReportJSON 把导出记录写为 JSON 文件（v0.2.20 --report）
func writeReportJSON(o *options, path string, dbOid int, records []tableReportRecord) {
	// 按表名排序，保证输出确定性（并行 worker 的 records append 顺序不确定）
	sort.Slice(records, func(i, j int) bool {
		if records[i].Schema != records[j].Schema {
			return records[i].Schema < records[j].Schema
		}
		return records[i].Name < records[j].Name
	})
	type reportStruct struct {
		Version string              `json:"version"`
		Database int                `json:"database"`
		Tables  []tableReportRecord `json:"tables"`
	}
	r := reportStruct{Version: version, Database: dbOid, Tables: records}
	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: 生成报告 JSON 失败:", err)
		return
	}
	if err := os.WriteFile(path, buf, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "warning: 写报告文件失败:", err)
		return
	}
	vlog(o, "导出报告已写入: %s (%d 张表)", path, len(records))
}

// generateImportSh 在批量导出目录生成 import.sh（v0.2.20）。
// 按文件名排序，.ddl 文件排在 .sql/.csv 之前；每行一个 gsql -f 调用。
func generateImportSh(o *options, outDir string, ext string) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "import.sh" {
			continue
		}
		// 只收集本次导出产生的数据文件
		if strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".csv") || strings.HasSuffix(name, ".ddl") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return
	}
	// 排序：.ddl 在前，其余按文件名
	sort.Slice(files, func(i, j int) bool {
		di, dj := strings.HasSuffix(files[i], ".ddl"), strings.HasSuffix(files[j], ".ddl")
		if di != dj {
			return di // .ddl 排前面
		}
		return files[i] < files[j]
	})

	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# gauss2sql-go v" + version + " 自动生成的导入脚本\n")
	sb.WriteString("# 用法: TARGET_DB=<目标库名> gsql -d $TARGET_DB -f import.sh\n")
	sb.WriteString("#   或: gsql -d <目标库> -f <某个 .sql/.ddl 文件>\n")
	sb.WriteString("set -e\n\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("gsql -f %q\n", f))
	}

	path := filepath.Join(outDir, "import.sh")
	if err := os.WriteFile(path, []byte(sb.String()), 0755); err != nil {
		fmt.Fprintln(os.Stderr, "warning: 生成 import.sh 失败:", err)
		return
	}
	vlog(o, "import.sh 已生成: %s (%d 个文件)", path, len(files))
}

// isSystemSchema 判断 openGauss 系统 schema（默认 --export-db 时排除）。
// 系统 schema：pg_* 前缀、information_schema、openGauss 扩展 dbe_*/coverage/db4ai/snapshot、
// 以及 openGauss 系统包 schema pkg_*（v0.2.16）。
func isSystemSchema(s string) bool {
	if strings.HasPrefix(s, "pg_") || strings.HasPrefix(s, "dbe_") || strings.HasPrefix(s, "pkg_") {
		return true
	}
	switch s {
	case "information_schema", "coverage", "db4ai", "snapshot":
		return true
	}
	return false
}

// buildSetvalStmts 生成 setval 序列同步语句（在数据导入后执行，自增列从当前最大值继续）。
// 仅处理 DEFAULT nextval(...) 的列；裸序列名按当前 schema 解析。
func buildSetvalStmts(tm *meta.TableMeta, dbDir string) []string {
	if tm.Reloid == 0 {
		return nil
	}
	defaults := catalog.LoadAttrDefaults(dbDir, tm.Reloid, 0)
	if len(defaults) == 0 {
		return nil
	}
	nameByNum := map[int]string{}
	for _, c := range tm.Columns {
		if !c.Attdropped {
			nameByNum[c.Attnum] = c.Name
		}
	}
	seqRe := regexp.MustCompile(`nextval\('([^']+)'::regclass\)`)
	var out []string
	attNums := make([]int, 0, len(defaults))
	for n := range defaults {
		attNums = append(attNums, n)
	}
	sort.Ints(attNums)
	for _, n := range attNums {
		m := seqRe.FindStringSubmatch(defaults[n])
		if len(m) != 2 {
			continue
		}
		col, ok := nameByNum[n]
		if !ok {
			continue
		}
		seq := m[1]
		if !strings.Contains(seq, ".") {
			seq = tm.Schema + "." + seq
		}
		// 空表：setval(seq,1,false) → 下一个 id=1（与 serial 从 1 起始一致）；
		// 非空表：setval(MAX,true) → 下一个 id=MAX+1
		out = append(out, fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT MAX(\"%s\") FROM \"%s\".\"%s\"), 1), (SELECT MAX(\"%s\") FROM \"%s\".\"%s\") IS NOT NULL);",
			seq, col, tm.Schema, tm.Relname, col, tm.Schema, tm.Relname))
	}
	return out
}

// vlog：--verbose 时向 stderr 打印导出信息（不污染输出文件/管道）
func vlog(o *options, format string, args ...interface{}) {
	if o.verbose {
		fmt.Fprintf(os.Stderr, "[verbose] "+format+"\n", args...)
	}
}

// 默认导出文件名：schema.对象名.类型（sql 或 csv）
func defaultOutName(tm *meta.TableMeta, ext string) string {
	return tm.Schema + "." + tm.Relname + "." + ext
}

// resolveOutput：-o/--output 解析（与 Python 版行为一致）
//   - 未指定          -> 返回空串，结果输出到标准输出
//   - 指定为目录(/)结尾或已存在目录 -> 该目录下生成 <schema>.<对象名>.<ext>
//   - 指定带扩展名的路径 -> 原样使用
//   - 指定不带扩展名的路径 -> 追加 .<ext>
func resolveOutput(output string, tm *meta.TableMeta, ext string) string {
	if output == "" {
		return ""
	}
	if strings.HasSuffix(output, string(os.PathSeparator)) {
		return filepath.Join(output, defaultOutName(tm, ext))
	}
	if fi, err := os.Stat(output); err == nil && fi.IsDir() {
		return filepath.Join(output, defaultOutName(tm, ext))
	}
	if filepath.Ext(output) != "" {
		return output
	}
	return output + "." + ext
}

func printHelp() {
	fmt.Printf(`gauss2sql-go %s (Author: raysuen)
离线解析 openGauss 堆数据文件并导出为 SQL / CSV / DDL / 元数据 JSON

用法:
  gauss2sql-go <数据文件或数据目录> [选项]

位置参数:
  <data-file-or-db-dir>    openGauss 堆数据文件路径 (如 base/16388/16414)

通用选项:
  -h, --help               显示此帮助信息并退出
  --version                显示版本号并退出
  --datadir DIR            openGauss 数据目录 (配合 --list-db; 或配合 --db-id 直接定位单库导出)
  --db-id OID              数据库 OID (与 --datadir 组合: dbDir = <datadir>/base/<OID>,
                           驱动 --list-tables-db/--table-name/--schema/--tables 等导出模式;
                           与位置参数并存、向后兼容, 老命令 gauss2sql base/16388 ... 行为不变)

元数据 (可选, 不指定则自动发现):
  --catalog-json FILE      从 JSON 文件加载元数据 (由 --export-meta 导出)
  --table-name NAME        指定表名 (schema.table 或 table)
                           配合 --catalog-json: 从 JSON 按名匹配表
                           位置参数为数据库目录时: 按名直查系统目录定位表并导出
                           (支持分区父表, 自动展开全部子分区; v0.2.18 起支持子分区名单独导出;
                            v0.2.19 起子分区按自身 reltablespace 解析独立表空间文件;
                            裸表名需全库唯一, 普通表优先于同名子分区)
                           表空间自动发现 (v0.2.16): 独立表空间里的表经 pg_tblspc
                           符号链接解析真实文件路径, TOAST 大字段随表空间一同定位

输出模式:
  --ddl                    输出 CREATE TABLE DDL (含列默认值/主键/唯一/CHECK约束/表列注释/
                           CREATE SEQUENCE/setval/非主键索引)
  --sql                    输出 INSERT 语句 (数据后追加 setval 序列同步)
  --data                   输出 CSV 格式 (可用 COPY 导入; --delimiter 指定分隔符)
  --deleted                输出已删除和未删除的行 (t_xmax 已设置但未被 vacuum 清理)
  --only-deleted           只输出已删除的行
  --count                  仅统计行数, 不输出数据
  --list-db                列出数据目录中的所有数据库 (OID + 名称 + 目录路径;
                           v0.2.16 起多行追加该库在独立表空间下的目录路径)
  --list-tables-db         列出指定数据库目录中的用户对象 (普通表/索引/序列/TOAST/视图)
  --list-tables-all        列出指定数据库目录中的全部对象 (含系统 schema)
  --export-meta            导出元数据 JSON

批量导出 (位置参数为数据库目录, 必须 -o 目录 + --sql/--data/--ddl 之一):
  --tables                 批量导出全部用户普通表 (v0.2.18 起自动展开分区父表)
  --all-tables             批量导出全部普通表 (含系统 schema; 含分区父表展开)
  --schema NAME            按 schema 过滤批量导出 (逗号分隔多值, 等价 --tables --schema)

输出选项:
  -o, --output PATH        输出到文件或目录
                           未指定时输出到标准输出 (批量导出模式必须指定目录)
                           目录(以/结尾或已存在)则生成 <路径>/<schema>.<对象名>.<sql|csv|ddl>
                           不带扩展名的路径自动追加 .sql/.csv
  --parallel N             并行线程数 (默认 1, 输出与串行逐字节一致)
  --limit N                只导出前 N 行
  --delimiter STR          CSV 字段分隔符 (默认 ",")
  --fields COL1,COL2       只导出指定字段 (逗号分隔)
  --header                 CSV 首行输出字段名 (配合 --data, 与 COPY HEADER true 兼容)
  --encoding ENC           库数据解码编码 (自动探测; 探测失败时可指定
                           UTF8/GBK/GB18030/LATIN1/SQL_ASCII 等, 输出统一为 UTF-8)
  --verbose                打印导出信息 (表结构/TOAST/输出路径/行数/耗时, 输出到 stderr)

v0.2.20 新增选项 (默认均关闭, 与历史版本输出逐字节一致):
  --batch-rows N           SQL 模式下每 N 行合并为一条多值 INSERT (列名只写一次,
                           形如 INSERT INTO s.t (c) VALUES (r1),(r2),...;);
                           默认 0 = 每行一条 INSERT, 与历史输出逐字节一致
  --jobs M                 批量导出 (--tables/--all-tables/--schema) 表间并行 worker 数;
                           默认 1 = 串行, 与历史输出逐字节一致; 与 --parallel 正交可叠乘
  --report PATH            导出结束后在 PATH 写每表统计 JSON (schema/name/oid/rows/bytes/
                           sha256/duration_ms/corrupt_pages); 默认空 = 不写报告

示例:
  gauss2sql-go base/16388/16414 --ddl --sql
  gauss2sql-go base/16388/16414 --data --header -o /tmp/
  gauss2sql-go base/16388 --table-name ray.test_50col --sql --ddl -o /tmp/gauss2sql
  gauss2sql-go base/16388 --table-name test_50col --data --header -o /tmp/gauss2sql
  gauss2sql-go base/16388 --tables --schema app --sql -o /tmp/out
  gauss2sql-go base/16388 --all-tables --ddl -o /tmp/ddl
  gauss2sql-go base/16388 --list-tables-db
  gauss2sql-go --datadir /data/openGauss --list-db
  gauss2sql-go --datadir /data/openGauss --db-id 16385 --list-tables-db
  gauss2sql-go --datadir /data/openGauss --db-id 16385 --table-name app.t_in_tbs1 --sql
`, version)
}

func parseFields(s string) map[string]bool {
	m := map[string]bool{}
	if s == "" {
		return m
	}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			m[f] = true
		}
	}
	return m
}

func typeSQLName(tm *meta.TableMeta, oid int) string {
	if name, ok := tm.TypeNames[oid]; ok && name != "" {
		return name
	}
	if name, ok := typeNamesFallback[oid]; ok {
		return name
	}
	return "?"
}

var typeNamesFallback = map[int]string{
	16: "bool", 17: "bytea", 19: "name", 20: "bigint", 21: "smallint",
	23: "integer", 25: "text", 26: "oid", 27: "tid", 28: "xid", 29: "cid",
	114: "json", 650: "cidr", 700: "real", 701: "double precision",
	790: "money", 829: "macaddr", 869: "inet", 1042: "character",
	1043: "character varying", 1082: "date", 1083: "time", 1114: "timestamp",
	1184: "timestamptz", 1186: "interval", 1560: "bit", 1562: "varbit",
	1700: "numeric", 2950: "uuid", 3802: "jsonb",
	600: "point", 601: "lseg", 602: "path", 603: "box", 604: "polygon",
	628: "line", 718: "circle", 774: "macaddr8", 142: "xml",
	24: "regproc", 2202: "regprocedure", 2203: "regoper", 2204: "regoperator",
	2205: "regclass", 2206: "regtype", 3734: "regconfig", 3769: "regdictionary",
	4089: "regnamespace", 4096: "regrole", 4191: "regcollation",
	3220: "pg_lsn", 3614: "tsvector", 3615: "tsquery",
	3904: "int4range", 3906: "numrange", 3908: "tsrange", 3910: "tstzrange",
	3912: "daterange", 3926: "int8range", 5030: "txid_snapshot",
}

func colTypeSQL(tm *meta.TableMeta, c *meta.Column) string {
	base := tm.TypeNames[c.Atttypid]
	if base == "" {
		base = "oid:" + strconv.Itoa(c.Atttypid)
	}
	switch c.Atttypid {
	case 1042, 1043: // bpchar, varchar
		if c.Typmod >= 4 {
			return base + "(" + strconv.Itoa(c.Typmod-4) + ")"
		}
		return base
	case 1700: // numeric
		if c.Typmod >= 4 {
			tmp := c.Typmod - 4
			prec := (tmp >> 16) & 0xFFFF
			scale := tmp & 0xFFFF
			return "numeric(" + strconv.Itoa(prec) + "," + strconv.Itoa(scale) + ")"
		}
		return "numeric"
	case 1560, 1562: // bit, varbit
		if c.Typmod != 0 {
			return base + "(" + strconv.Itoa(c.Typmod) + ")"
		}
		return base
	case 1083, 1266, 1114, 1184: // time(tz), timestamp(tz)
		if c.Typmod != 0 && c.Typmod >= 0 {
			return base + "(" + strconv.Itoa(c.Typmod) + ")"
		}
		return base
	}
	return base
}

// buildDDL 生成 CREATE TABLE（含默认值/表级约束/注释；catalog-json 模式 Reloid=0 时仅基础列定义）
func buildDDL(tm *meta.TableMeta, dbDir string) string {
	var sb strings.Builder
	// 枚举列前置 CREATE TYPE
	seenEnum := map[int]bool{}
	for _, c := range tm.Columns {
		if c.Attdropped {
			continue
		}
		labels := types.EnumLabels(c.Atttypid)
		if len(labels) == 0 || seenEnum[c.Atttypid] {
			continue
		}
		seenEnum[c.Atttypid] = true
		tname := colTypeSQL(tm, c)
		var quoted []string
		for _, lab := range labels {
			quoted = append(quoted, "'"+strings.ReplaceAll(lab, "'", "''")+"'")
		}
		sb.WriteString(`CREATE TYPE "` + tname + `" AS ENUM (` + strings.Join(quoted, ", ") + ");\n")
	}

	// 默认值 / 约束 / 注释（需 Reloid）
	var defaults map[int]string
	var constraints []catalog.ConstraintInfo
	var comments map[int]string
	if tm.Reloid != 0 {
		defaults = catalog.LoadAttrDefaults(dbDir, tm.Reloid, 0)
		constraints = catalog.LoadConstraints(dbDir, tm.Reloid, 0)
		comments = catalog.LoadDescriptions(dbDir, tm.Reloid, 0)
	}
	// 序列前置：DEFAULT nextval(...) 引用的序列必须先于 CREATE TABLE 创建
	if len(defaults) > 0 {
		seqs := catalog.LoadSequences(dbDir, 0)
		seqRe := regexp.MustCompile(`nextval\('([^']+)'::regclass\)`)
		seenSeq := map[string]bool{}
		attNums := make([]int, 0, len(defaults))
		for n := range defaults {
			attNums = append(attNums, n)
		}
		sort.Ints(attNums)
		for _, n := range attNums {
			m := seqRe.FindStringSubmatch(defaults[n])
			if len(m) != 2 {
				continue
			}
			seq := m[1]
			if !strings.Contains(seq, ".") {
				seq = tm.Schema + "." + seq
			}
			if seenSeq[seq] {
				continue
			}
			seenSeq[seq] = true
			// 裸名 → 当前 schema；带 schema 名直接使用
			schema, name := tm.Schema, seq
			if i := strings.IndexByte(seq, '.'); i >= 0 {
				schema, name = seq[:i], seq[i+1:]
			}
			_ = seqs // 目录内序列名映射已由 pg_class 校验，此处直接输出
			sb.WriteString("CREATE SEQUENCE IF NOT EXISTS \"" + schema + "\".\"" + name + "\";\n")
		}
	}

	nameByNum := map[int]string{}
	var colDefs []string
	for _, c := range tm.Columns {
		if c.Attdropped {
			continue
		}
		nameByNum[c.Attnum] = c.Name
		def := "  \"" + c.Name + "\" " + colTypeSQL(tm, c)
		if d, ok := defaults[c.Attnum]; ok {
			def += " DEFAULT " + d
		}
		if c.Notnull {
			def += " NOT NULL"
		}
		colDefs = append(colDefs, def)
	}
	// 表级约束（主键/唯一/CHECK）
	var tableCons []string
	for _, ci := range constraints {
		var cols []string
		for _, n := range ci.Cols {
			if name, ok := nameByNum[n]; ok {
				cols = append(cols, "\""+name+"\"")
			}
		}
		switch ci.Type {
		case 'p':
			if len(cols) > 0 {
				tableCons = append(tableCons, "  CONSTRAINT \""+ci.Name+"\" PRIMARY KEY ("+strings.Join(cols, ", ")+")")
			}
		case 'u':
			if len(cols) > 0 {
				tableCons = append(tableCons, "  CONSTRAINT \""+ci.Name+"\" UNIQUE ("+strings.Join(cols, ", ")+")")
			}
		case 'c':
			if ci.Src != "" {
				tableCons = append(tableCons, "  CONSTRAINT \""+ci.Name+"\" CHECK ("+ci.Src+")")
			}
		}
	}
	sb.WriteString("CREATE TABLE \"" + tm.Schema + "\".\"" + tm.Relname + "\" (\n")
	sb.WriteString(strings.Join(colDefs, ",\n"))
	if len(tableCons) > 0 {
		sb.WriteString(",\n")
		sb.WriteString(strings.Join(tableCons, ",\n"))
	}
	sb.WriteString("\n);")
	// 非主键索引（含唯一索引；表达式/部分索引跳过；openGauss 默认 USING btree）
	if tm.Reloid != 0 {
		for _, ix := range catalog.LoadIndexes(dbDir, tm.Reloid, 0) {
			var cols []string
			exprIndex := false
			for _, n := range ix.Keys {
				if n <= 0 {
					exprIndex = true
					break
				}
				if nm, ok := nameByNum[n]; ok {
					cols = append(cols, "\""+nm+"\"")
				}
			}
			if exprIndex || len(cols) == 0 {
				continue
			}
			uniq := ""
			if ix.Unique {
				uniq = "UNIQUE "
			}
			sb.WriteString("\nCREATE " + uniq + "INDEX \"" + ix.Name + "\" ON \"" + tm.Schema + "\".\"" + tm.Relname + "\" (" + strings.Join(cols, ", ") + ");")
		}
	}
	// 注释
	for _, c := range tm.Columns {
		if c.Attdropped {
			continue
		}
		if desc, ok := comments[c.Attnum]; ok && desc != "" {
			sb.WriteString("\nCOMMENT ON COLUMN \"" + tm.Schema + "\".\"" + tm.Relname + "\".\"" + c.Name + "\" IS '" + strings.ReplaceAll(desc, "'", "''") + "';")
		}
	}
	if desc, ok := comments[0]; ok && desc != "" {
		sb.WriteString("\nCOMMENT ON TABLE \"" + tm.Schema + "\".\"" + tm.Relname + "\" IS '" + strings.ReplaceAll(desc, "'", "''") + "';")
	}
	return sb.String()
}

func loadCatalogJSON(path, tableName, targetFile string) *meta.TableMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	targetRF := 0
	if n, err := strconv.Atoi(targetFile); err == nil {
		targetRF = n
	}
	// 枚举注入：成员映射（数据解码）+ 有序标签（DDL CREATE TYPE 重建）
	if enums, ok := m["enums"].(map[string]interface{}); ok {
		lookup := map[int]map[int]string{}
		for toid, members := range enums {
			tOid := atoiSafe(toid)
			mm := map[int]string{}
			if mmap, ok := members.(map[string]interface{}); ok {
				for moid, lbl := range mmap {
					if s, ok := lbl.(string); ok {
						mm[atoiSafe(moid)] = s
					}
				}
			}
			lookup[tOid] = mm
		}
		types.SetEnumMap(lookup)
	}
	if labels, ok := m["enum_labels"].(map[string]interface{}); ok {
		lsMap := map[int][]string{}
		for toid, ls := range labels {
			var arr []string
			if arrI, ok := ls.([]interface{}); ok {
				for _, v := range arrI {
					if s, ok := v.(string); ok {
						arr = append(arr, s)
					}
				}
			}
			lsMap[atoiSafe(toid)] = arr
		}
		types.SetEnumLabels(lsMap)
	}
	tables, _ := m["tables"].([]interface{})
	for _, ti := range tables {
		t, ok := ti.(map[string]interface{})
		if !ok {
			continue // meta.json 结构不符条目：跳过而非 panic
		}
		name, _ := t["table"].(string)
		if tableName != "" {
			if name != tableName {
				continue
			}
		} else if targetRF > 0 {
			// 未指定表名时按位置参数文件的 relfilenode 匹配
			if int(getF(t, "relfilenode")) != targetRF {
				continue
			}
		}
		schema, _ := t["schema"].(string)
		rfn, _ := t["relfilenode"].(float64)
		relkind, _ := t["relkind"].(string)
		toastrelid := int(getF(t, "toastrelid"))
		var pk []string
		if pki, ok := t["primary_key"].([]interface{}); ok {
			for _, v := range pki {
				if s, ok := v.(string); ok {
					pk = append(pk, s)
				}
			}
		}
		typeNames := map[int]string{}
		if tni, ok := t["type_names"].(map[string]interface{}); ok {
			for toid, tn := range tni {
				if s, ok := tn.(string); ok {
					typeNames[atoiSafe(toid)] = s
				}
			}
		}
		var cols []*meta.Column
		rawCols, _ := t["columns"].([]interface{})
		for _, ci := range rawCols {
			cc, ok := ci.(map[string]interface{})
			if !ok {
				continue
			}
			cols = append(cols, &meta.Column{
				Name:       getStr(cc, "name"),
				Atttypid:   int(getF(cc, "type_oid")),
				Attlen:     int(getF(cc, "len")),
				Attnum:     int(getF(cc, "attnum")),
				Typmod:     int(getF(cc, "typmod")),
				Notnull:    getBool(cc, "notnull"),
				Attdropped: getBool(cc, "dropped"),
				Attalign:   getStr(cc, "attalign"),
				Attbyval:   getBool(cc, "attbyval"),
				Attstorage: getStr(cc, "attstorage"),
			})
		}
		return &meta.TableMeta{
			Schema: schema, Relname: name, Relfilenode: int(rfn),
			Reloid: int(getF(t, "oid")), Relkind: relkind, Toastrelid: toastrelid,
			PrimaryKey: pk, TypeNames: typeNames, Columns: cols,
		}
	}
	return nil
}

// atoiSafe 字符串转 int（失败返回 0）
func atoiSafe(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func getStr(m map[string]interface{}, k string) string { v, _ := m[k].(string); return v }
func getF(m map[string]interface{}, k string) float64  { v, _ := m[k].(float64); return v }
func getBool(m map[string]interface{}, k string) bool  { v, _ := m[k].(bool); return v }

// tablespaceDBDirs 扫描 $PGDATA/pg_tblspc/ 下每个数字 OID 符号链接，
// 解析目标 → 找 PG_ 版本子目录 → 若存在 <dbOid> 子目录则收集该表空间内的库目录路径（v0.2.16）。
// 无独立表空间或该库未在某表空间建对象时返回空切片（--list-db 输出与旧版逐字一致）。
func tablespaceDBDirs(pgdata string, dbOid int) []string {
	tbsRoot := filepath.Join(pgdata, "pg_tblspc")
	entries, err := os.ReadDir(tbsRoot)
	if err != nil {
		return nil
	}
	dbOidStr := strconv.Itoa(dbOid)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if _, err := strconv.Atoi(name); err != nil {
			continue // 跳过非数字条目
		}
		target, err := os.Readlink(filepath.Join(tbsRoot, name))
		if err != nil {
			continue
		}
		sub, err := os.ReadDir(target)
		if err != nil {
			continue
		}
		verDir := ""
		for _, se := range sub {
			if se.IsDir() && strings.HasPrefix(se.Name(), "PG_") {
				verDir = se.Name()
				break
			}
		}
		if verDir == "" {
			continue
		}
		dir := filepath.Join(target, verDir, dbOidStr)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

func listDBs(o *options) {
	// 确定 base/ 目录
	var baseDir string
	if o.datadir != "" {
		baseDir = filepath.Join(o.datadir, "base")
	} else if o.dataPath != "" {
		baseDir = o.dataPath
	} else {
		fmt.Fprintln(os.Stderr, "--list-db 需要指定数据目录路径")
		os.Exit(1)
	}
	var err error
	baseDir, err = filepath.Abs(baseDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if fi, err := os.Stat(baseDir); err != nil || !fi.IsDir() {
		fmt.Fprintln(os.Stderr, "目录不存在:", baseDir)
		os.Exit(1)
	}
	pgdata := filepath.Dir(baseDir)

	// 扫描数字子目录
	var dbOids []int
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // 跳过 pgsql_tmp 等非数字目录
		}
		dbOids = append(dbOids, n)
	}
	sort.Ints(dbOids)
	if len(dbOids) == 0 {
		return
	}

	dbNames := catalog.LoadDBNames(pgdata)

	if len(dbNames) > 0 {
		fmt.Printf("%-12s %-25s %s\n", "OID", "数据库名称", "目录路径")
		fmt.Println(strings.Repeat("-", 70))
		for _, oid := range dbOids {
			name, ok := dbNames[oid]
			if !ok {
				name = "(未知)"
			}
			fmt.Printf("%-12d %-25s %s\n", oid, name, filepath.Join(baseDir, strconv.Itoa(oid)))
			// v0.2.16：追加该库在独立表空间下的目录（多行显示）；无表空间时无附加行（向后兼容）
			for _, d := range tablespaceDBDirs(pgdata, oid) {
				fmt.Printf("%-38s %s\n", "", d)
			}
		}
		seen := map[int]bool{}
		for _, oid := range dbOids {
			seen[oid] = true
		}
		var extra []int
		for oid := range dbNames {
			if !seen[oid] {
				extra = append(extra, oid)
			}
		}
		sort.Ints(extra)
		for _, oid := range extra {
			fmt.Printf("%-12d %-25s %s\n", oid, dbNames[oid], "(base/ 下无对应目录)")
		}
	} else {
		fmt.Printf("%-12s %s\n", "OID", "目录路径")
		fmt.Println(strings.Repeat("-", 50))
		for _, oid := range dbOids {
			fmt.Printf("%-12d %s\n", oid, filepath.Join(baseDir, strconv.Itoa(oid)))
			// v0.2.16：追加该库在独立表空间下的目录（多行显示）；无表空间时无附加行
			for _, d := range tablespaceDBDirs(pgdata, oid) {
				fmt.Printf("%-13s %s\n", "", d)
			}
		}
		fmt.Println()
	}
}

func listTablesDB(dbDir string, all bool) {
	abs, _ := filepath.Abs(dbDir)
	dbOid := filepath.Base(abs)
	relkindNames := map[string]string{
		"r": "普通表", "v": "视图", "m": "物化视图", "i": "索引",
		"S": "序列", "c": "复合类型", "t": "TOAST表", "p": "分区表",
	}
	// v0.2.16：正向遍历 pg_class（旧"扫 base/<dbOid> 数字文件"方式会丢失独立表空间里的表）。
	// 以 pg_class 返回的全部表为准，用 reltablespace 经 pg_tblspc 解析真实文件路径。
	allTables := catalog.AutoDiscoverAllTablesOrdered(abs, 0)
	// 先登记全部 pg_class relfilenode（含系统 schema），避免 base 下系统目录文件被误判为孤立文件
	seenRFN := map[int]bool{}
	for _, tm := range allTables {
		seenRFN[tm.Relfilenode] = true
	}
	type row struct {
		rfn  int
		name string
		kind string
		path string
	}
	var rows []*row
	for _, tm := range allTables {
		if !all && isSystemSchema(tm.Schema) {
			continue // --list-tables-db 默认只列用户对象
		}
		kindStr := relkindNames[tm.Relkind]
		if kindStr == "" {
			kindStr = tm.Relkind
		}
		// 表空间感知定位：独立表空间里的表文件经 pg_tblspc 符号链接解析；reltablespace=0 即 base/<dbOid>/<rfn>
		path := catalog.ResolvePhysicalFile(abs, tm.Reltablespace, tm.Relfilenode)
		if path == "" {
			path = filepath.Join(abs, strconv.Itoa(tm.Relfilenode))
		}
		// v0.2.17：先 stat 物理文件是否真实存在。视图/共享表(文件在 global/ 不在 base/<dboid>)/
		// 复合类型等无对应物理文件的对象不输出幽灵行；只有真实存在的文件才 append 成 row。
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			continue
		}
		rows = append(rows, &row{tm.Relfilenode, tm.Schema + "." + tm.Relname, kindStr, path})
	}
	// 保留旧行为：base/<dbOid> 下不在 pg_class 的孤立数字文件显示为"(未知/系统表)"，
	// 保证无独立表空间的库 --list-tables-db 输出与旧版逐字一致。
	entries, err := os.ReadDir(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, "_fsm") || strings.HasSuffix(name, "_vm") || strings.HasSuffix(name, "_init") {
			continue
		}
		n, err := strconv.Atoi(name)
		if err != nil || seenRFN[n] {
			continue
		}
		rows = append(rows, &row{n, "(未知/系统表)", "-", filepath.Join(abs, strconv.Itoa(n))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].rfn < rows[j].rfn })
	fmt.Printf("数据库 OID: %s\n", dbOid)
	fmt.Printf("%-14s %-40s %-10s %s\n", "relfilenode", "表名", "类型", "文件路径")
	fmt.Println("------------------------------------------------------------------------------------------")
	for _, r := range rows {
		fmt.Printf("%-14d %-40s %-10s %s\n", r.rfn, r.name, r.kind, r.path)
	}
}

func exportMeta(dbDir string, o *options) {
	j := catalog.ExportMetaToJSON(dbDir, 0)
	buf, err := catalog.MarshalIndent(j)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	out := o.output
	if out != "" {
		if strings.HasSuffix(out, string(os.PathSeparator)) {
			out = filepath.Join(out, filepath.Base(dbDir)+".meta.json")
		} else if fi, err := os.Stat(out); err == nil && fi.IsDir() {
			out = filepath.Join(out, filepath.Base(dbDir)+".meta.json")
		}
	}
	writeOutput(out, string(buf))
}

func writeOutput(path, s string) {
	if path == "" {
		// stdout 模式等价 Python print()，末尾补换行
		fmt.Print(s)
		if !strings.HasSuffix(s, "\n") {
			fmt.Println()
		}
		return
	}
	if err := os.WriteFile(path, []byte(s), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
