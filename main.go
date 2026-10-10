// gauss2sql-go 命令行入口（Author: raysuen）
// 移植自 gauss2sql/main.py：离线解析 openGauss 堆文件，生成 DDL/SQL/CSV/meta。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gauss2sql-go/internal/binary"
	"gauss2sql-go/internal/catalog"
	"gauss2sql-go/internal/heapfile"
	"gauss2sql-go/internal/meta"
	"gauss2sql-go/internal/toast"
	"gauss2sql-go/internal/types"
)

var version = "0.2.15"

type options struct {
	dataPath      string
	datadir       string
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
	// --list-db 可仅用 --datadir；其余模式需要位置参数 dataPath
	if o.dataPath == "" && !o.listDB {
		fmt.Fprintln(os.Stderr, "usage: gauss2sql-go <data-file-or-db-dir> [options]")
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

	abs, _ := filepath.Abs(o.dataPath)
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
	w, done, outPath := openOutWriter(o, tm, ext)
	nRows, _ := streamTable(abs, o, w)
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
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
	w, done, outPath := openOutWriter(o, tm, ext)
	nRows, _ := streamTable(filePath, o, w)
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
}

// exportPartitionParent 分区父表导出（v0.2.13 流式）：DDL 使用父表列定义输出一次，
// 数据按全部子分区文件逐个流式解析并输出（行顺序 = pg_partition 扫描顺序），最后统一追加 setval。
func exportPartitionParent(dbDir string, tm *meta.TableMeta, o *options) {
	files := catalog.ListPartitionFiles(dbDir, tm.Reloid)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: 分区父表 %s 无子分区数据文件\n", o.tableName)
		os.Exit(1)
	}
	vlog(o, "分区父表: 展开 %d 个子分区文件", len(files))

	fields := parseFields(o.fields)
	if len(fields) > 0 {
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

	if o.count {
		total := 0
		for _, f := range files {
			sub := catalog.AutoDiscoverMeta(f, 0)
			if sub == nil {
				continue
			}
			hf := subHeapFile(f, o, dbDir, tm, sub)
			n, _ := hf.StreamRowsCount(sub, o.deleted, o.onlyDeleted, o.limit)
			total += n
		}
		fmt.Printf("-- 总行数: %d\n", total)
		return
	}

	ext := "sql"
	if o.data {
		ext = "csv"
	}
	start := time.Now()
	w, done, outPath := openOutWriter(o, tm, ext)
	defer done()
	var nRows int
	if o.ddl {
		fmt.Fprintf(w, "%s\n\n", buildDDL(tm, dbDir))
	}
	for _, f := range files {
		sub := catalog.AutoDiscoverMeta(f, 0)
		if sub == nil {
			fmt.Fprintf(os.Stderr, "warning: 子分区文件无法解析结构, 跳过: %s\n", f)
			continue
		}
		if sub.Toastrelid == 0 {
			sub.Toastrelid = tm.Toastrelid // 分区表 TOAST 在父表上，子分区继承
		}
		hf := subHeapFile(f, o, dbDir, tm, sub)
		if o.sql {
			n, err := hf.StreamSQL(sub, o.deleted, o.onlyDeleted, o.limit, true, false, fields, writeLine(w))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			nRows = n
		}
		if o.data {
			n, err := hf.StreamToData(sub, o.deleted, o.onlyDeleted, o.limit, o.delimiter, fields, o.header, writeLine(w))
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			nRows = n
		}
	}
	if o.sql {
		for _, s := range buildSetvalStmts(tm, dbDir) {
			fmt.Fprintln(w, s)
		}
	}
	done()
	verboseReport(o, tm, outPath, nRows, ext, start)
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

// streamTable 流式导出单表（DDL/SQL/CSV/setval 直接写入 w，v0.2.13 A+B：内存 O(块)）。
// 返回行数与元数据；行数语义与旧版一致（data 覆盖 sql）。
func streamTable(abs string, o *options, w io.Writer) (int, *meta.TableMeta) {
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
		n, err := hf.StreamSQL(tm, o.deleted, o.onlyDeleted, o.limit, true, false, fields, writeLine(w))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
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
	return nRows, tm
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

// exportDBAll 批量导出数据库（或 --schema 过滤的 schema）下所有普通表（对齐 pg2sql --tables/--all-tables）。
// 位置参数为数据库目录（如 base/16388）；必须指定 -o 输出目录且 --sql/--data/--ddl 之一；
// 每表一个 <schema>.<对象名>.<sql|csv|ddl> 文件；--all-tables 时包含系统 schema。
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
		if tm.Relkind != "r" { // 仅普通表（含分区子分区），跳过索引/序列/TOAST/视图
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
		fmt.Fprintln(os.Stderr, "error: 未发现可导出的普通表（指定 --schema 或检查数据库目录）")
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
	total := 0
	for _, tm := range list {
		abs := filepath.Join(dbDir, strconv.Itoa(tm.Relfilenode))
		if _, err := os.Stat(abs); err != nil {
			// 数据文件缺失（如已 VACUUM FULL 换文件）：跳过并警告，不中断整个批量
			fmt.Fprintf(os.Stderr, "warning: 跳过 %s.%s（数据文件不存在: %s）\n", tm.Schema, tm.Relname, abs)
			continue
		}
		if o.count {
			n := countTable(abs, o, tm)
			fmt.Printf("-- %s.%s: %d 行\n", tm.Schema, tm.Relname, n)
			total += n
			continue
		}
		path := filepath.Join(o.output, defaultOutName(tm, ext))
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		w := bufio.NewWriterSize(f, 1<<20)
		nRows, _ := streamTable(abs, o, w)
		if err := w.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		_ = f.Close()
		total += nRows
		vlog(o, "已导出: %s (%d 行)", path, nRows)
	}
	if o.verbose {
		vlog(o, "批量导出完成: %d 张表, %d 行, 耗时 %.2fs", len(list), total, time.Since(start).Seconds())
	}
}

// isSystemSchema 判断 openGauss 系统 schema（默认 --export-db 时排除）。
// 系统 schema：pg_* 前缀、information_schema、openGauss 扩展 dbe_*/coverage/db4ai/snapshot。
func isSystemSchema(s string) bool {
	if strings.HasPrefix(s, "pg_") || strings.HasPrefix(s, "dbe_") {
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
  --datadir DIR            openGauss 数据目录 (配合 --list-db)

元数据 (可选, 不指定则自动发现):
  --catalog-json FILE      从 JSON 文件加载元数据 (由 --export-meta 导出)
  --table-name NAME        指定表名 (schema.table 或 table)
                           配合 --catalog-json: 从 JSON 按名匹配表
                           位置参数为数据库目录时: 按名直查系统目录定位表并导出
                           (支持分区父表, 自动展开全部子分区; 裸表名需全库唯一)

输出模式:
  --ddl                    输出 CREATE TABLE DDL (含列默认值/主键/唯一/CHECK约束/表列注释/
                           CREATE SEQUENCE/setval/非主键索引)
  --sql                    输出 INSERT 语句 (数据后追加 setval 序列同步)
  --data                   输出 CSV 格式 (可用 COPY 导入; --delimiter 指定分隔符)
  --deleted                输出已删除和未删除的行 (t_xmax 已设置但未被 vacuum 清理)
  --only-deleted           只输出已删除的行
  --count                  仅统计行数, 不输出数据
  --list-db                列出数据目录中的所有数据库 (OID + 名称 + 目录路径)
  --list-tables-db         列出指定数据库目录中的用户对象 (普通表/索引/序列/TOAST/视图)
  --list-tables-all        列出指定数据库目录中的全部对象 (含系统 schema)
  --export-meta            导出元数据 JSON

批量导出 (对齐 pg2sql, 位置参数为数据库目录, 必须 -o 目录 + --sql/--data/--ddl 之一):
  --tables                 批量导出全部用户普通表
  --all-tables             批量导出全部普通表 (含系统 schema)
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

示例:
  gauss2sql-go base/16388/16414 --ddl --sql
  gauss2sql-go base/16388/16414 --data --header -o /tmp/
  gauss2sql-go base/16388 --table-name ray.test_50col --sql --ddl -o /tmp/gauss2sql
  gauss2sql-go base/16388 --table-name test_50col --data --header -o /tmp/gauss2sql
  gauss2sql-go base/16388 --tables --schema app --sql -o /tmp/out
  gauss2sql-go base/16388 --all-tables --ddl -o /tmp/ddl
  gauss2sql-go base/16388 --list-tables-db
  gauss2sql-go --datadir /data/openGauss --list-db
`, version)
}

func parseFields(s string) map[string]bool {	m := map[string]bool{}
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
				Name:      getStr(cc, "name"),
				Atttypid:  int(getF(cc, "type_oid")),
				Attlen:    int(getF(cc, "len")),
				Attnum:    int(getF(cc, "attnum")),
				Typmod:    int(getF(cc, "typmod")),
				Notnull:   getBool(cc, "notnull"),
				Attdropped:getBool(cc, "dropped"),
				Attalign:  getStr(cc, "attalign"),
				Attbyval:  getBool(cc, "attbyval"),
				Attstorage:getStr(cc, "attstorage"),
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
func getF(m map[string]interface{}, k string) float64 { v, _ := m[k].(float64); return v }
func getBool(m map[string]interface{}, k string) bool { v, _ := m[k].(bool); return v }

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
		}
		fmt.Println()
	}
}

func listTablesDB(dbDir string, all bool) {
	abs, _ := filepath.Abs(dbDir)
	dbOid := filepath.Base(abs)
	// 扫描目录下数字文件
	var files []int
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
		if n, err := strconv.Atoi(name); err == nil {
			files = append(files, n)
		}
	}
	sort.Ints(files)
	// relfilenode → 表元信息（含 schema/relkind）
	byRFN := map[int]*meta.TableMeta{}
	for _, tm := range catalog.AutoDiscoverAllTablesOrdered(abs, 0) {
		byRFN[tm.Relfilenode] = tm
	}
	relkindNames := map[string]string{
		"r": "普通表", "v": "视图", "m": "物化视图", "i": "索引",
		"S": "序列", "c": "复合类型", "t": "TOAST表", "p": "分区表",
	}
	fmt.Printf("数据库 OID: %s\n", dbOid)
	fmt.Printf("%-14s %-40s %-10s %s\n", "relfilenode", "表名", "类型", "文件路径")
	fmt.Println("------------------------------------------------------------------------------------------")
	for _, rfn := range files {
		path := filepath.Join(abs, strconv.Itoa(rfn))
		tm, ok := byRFN[rfn]
		if !ok {
			fmt.Printf("%-14d %-40s %-10s %s\n", rfn, "(未知/系统表)", "-", path)
			continue
		}
		if !all && isSystemSchema(tm.Schema) {
			continue // --list-tables-db 默认只列用户对象（对齐 pg2sql）
		}
		kindStr := relkindNames[tm.Relkind]
		if kindStr == "" {
			kindStr = tm.Relkind
		}
		fmt.Printf("%-14d %-40s %-10s %s\n", rfn, tm.Schema+"."+tm.Relname, kindStr, path)
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
