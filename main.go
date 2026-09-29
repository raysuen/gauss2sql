// gauss2sql-go 命令行入口（Author: raysuen）
// 移植自 gauss2sql/main.py：离线解析 openGauss 堆文件，生成 DDL/SQL/CSV/meta。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

var version = "0.1.6"

type options struct {
	dataPath      string
	datadir       string
	listDB        bool
	listTablesDB  bool
	exportMeta    bool
	ddl           bool
	sql           bool
	data          bool
	output        string
	parallel      int
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
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--list-db":
			o.listDB = true
		case "--list-tables-db":
			o.listTablesDB = true
		case "--export-meta":
			o.exportMeta = true
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
			i++
			o.datadir = args[i]
		case "--output", "-o":
			i++
			o.output = args[i]
		case "--parallel", "-j":
			i++
			o.parallel, _ = strconv.Atoi(args[i])
		case "--fields":
			i++
			o.fields = args[i]
		case "--encoding":
			i++
			o.encoding = args[i]
		case "--catalog-json":
			i++
			o.catalogJSON = args[i]
		case "--table-name":
			i++
			o.tableName = args[i]
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
		listTablesDB(abs)
		return
	}
	if o.exportMeta {
		exportMeta(abs, o)
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

	var tm *meta.TableMeta
	if o.catalogJSON != "" {
		tm = loadCatalogJSON(o.catalogJSON, o.tableName)
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

	hf := heapfile.NewHeapFile(abs, 0)
	// TOAST：reltoastrelid 是 toast 表 OID，需映射到物理 relfilenode
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

	if o.count {
		rows := hf.DumpRows(tm, o.deleted, o.onlyDeleted, 0)
		if o.verbose {
			fmt.Fprintf(os.Stderr, "[verbose] 统计完成: %d 行\n", len(rows))
		}
		fmt.Printf("-- 总行数: %d\n", len(rows))
		return
	}

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

	var out strings.Builder
	if o.ddl {
		out.WriteString(buildDDL(tm))
		out.WriteString("\n\n")
	}
	if o.sql {
		for _, s := range hf.ToSQL(tm, o.deleted, o.onlyDeleted, 0, true, false, fields) {
			out.WriteString(s)
			out.WriteString("\n")
		}
	}
	if o.data {
		for _, line := range hf.ToData(tm, o.deleted, o.onlyDeleted, 0, ",", fields, o.header) {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}

	ext := "sql"
	if o.data {
		ext = "csv"
	}
	outPath := resolveOutput(o.output, tm, ext)
	start := time.Now()
	var nRows int
	if o.sql || o.ddl {
		nRows = len(hf.ToSQL(tm, o.deleted, o.onlyDeleted, 0, true, false, fields))
	}
	if o.data {
		nRows = len(hf.ToData(tm, o.deleted, o.onlyDeleted, 0, ",", fields, o.header))
	}
	writeOutput(outPath, out.String())
	if o.verbose {
		mode := "sql"
		if o.data {
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
  --table-name NAME        指定表名 (schema.table 或 table, 配合 --catalog-json)

输出模式:
  --ddl                    输出 CREATE TABLE DDL 语句
  --sql                    输出 INSERT 语句
  --data                   输出 CSV 格式 (可用 COPY 导入)
  --deleted                输出已删除和未删除的行 (t_xmax 已设置但未被 vacuum 清理)
  --only-deleted           只输出已删除的行
  --count                  仅统计行数, 不输出数据
  --list-db                列出数据目录中的所有数据库 (OID + 名称 + 目录路径)
  --list-tables-db         列出指定数据库目录中的所有表
  --export-meta            导出元数据 JSON

输出选项:
  -o, --output PATH        输出到文件或目录
                           未指定时输出到标准输出
                           目录(以/结尾或已存在)则生成 <路径>/<schema>.<对象名>.<sql|csv>
                           不带扩展名的路径自动追加 .sql/.csv
  --parallel N             并行线程数 (默认 1, 输出与串行逐字节一致)
  --fields COL1,COL2       只导出指定字段 (逗号分隔)
  --header                 CSV 首行输出字段名 (配合 --data, 与 COPY HEADER true 兼容)
  --encoding ENC           输出编码 (如 UTF8)
  --verbose                打印导出信息 (表结构/TOAST/输出路径/行数/耗时, 输出到 stderr)

示例:
  gauss2sql-go base/16388/16414 --ddl --sql
  gauss2sql-go base/16388/16414 --data --header -o /tmp/
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

func buildDDL(tm *meta.TableMeta) string {
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
	var colDefs []string
	for _, c := range tm.Columns {
		if c.Attdropped {
			continue
		}
		def := "  \"" + c.Name + "\" " + colTypeSQL(tm, c)
		if c.Notnull {
			def += " NOT NULL"
		}
		colDefs = append(colDefs, def)
	}
	sb.WriteString("CREATE TABLE \"" + tm.Schema + "\".\"" + tm.Relname + "\" (\n")
	sb.WriteString(strings.Join(colDefs, ",\n"))
	sb.WriteString("\n);")
	return sb.String()
}

func loadCatalogJSON(path, tableName string) *meta.TableMeta {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	// 简化：从 catalog json 重建
	tables, _ := m["tables"].([]interface{})
	for _, ti := range tables {
		t := ti.(map[string]interface{})
		name, _ := t["table"].(string)
		if tableName != "" && name != tableName {
			continue
		}
		schema, _ := t["schema"].(string)
		rfn, _ := t["relfilenode"].(float64)
		var cols []*meta.Column
		rawCols, _ := t["columns"].([]interface{})
		for _, ci := range rawCols {
			cc := ci.(map[string]interface{})
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
		return &meta.TableMeta{Schema: schema, Relname: name, Relfilenode: int(rfn), Columns: cols}
	}
	return nil
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

func listTablesDB(dbDir string) {
	abs, _ := filepath.Abs(dbDir)
	dbOid := filepath.Base(abs)
	// 扫描目录下数字文件
	type tableFile struct{ rfn int; path string }
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
	// pg_class 映射
	rfnToInfo := map[int][2]string{}
	classPath := catalog.SysFilePathPublic(abs, 1259)
	if classPath != "" {
		catalog.IterateClass(abs, 0, func(oid int, relname, relkind string, rfn int) {
			rfnToInfo[rfn] = [2]string{relname, relkind}
		})
	}
	relkindNames := map[string]string{
		"r": "普通表", "v": "视图", "m": "物化视图", "i": "索引",
		"S": "序列", "c": "复合类型", "t": "TOAST表",
	}
	fmt.Printf("数据库 OID: %s\n", dbOid)
	fmt.Printf("%-14s %-30s %-10s %s\n", "relfilenode", "表名", "类型", "文件路径")
	fmt.Println("------------------------------------------------------------------------------------------")
	for _, rfn := range files {
		path := filepath.Join(abs, strconv.Itoa(rfn))
		info, ok := rfnToInfo[rfn]
		if ok {
			relname, relkind := info[0], info[1]
			kindStr := relkindNames[relkind]
			if kindStr == "" {
				kindStr = relkind
			}
			fmt.Printf("%-14d %-30s %-10s %s\n", rfn, relname, kindStr, path)
		} else {
			fmt.Printf("%-14d %-30s %-10s %s\n", rfn, "(未知/系统表)", "-", path)
		}
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
