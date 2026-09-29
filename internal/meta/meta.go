// Package meta 移植自 gauss2sql/catalog.py 的 Column/TableMeta（Author: raysuen）
package meta

// Column 一列元数据
type Column struct {
	Name       string
	Atttypid   int
	Attlen     int
	Attnum     int
	Typmod     int
	Notnull    bool
	Attdropped bool
	Attalign   string
	Attbyval   bool
	Attstorage string
}

// TableMeta 一张表的元数据
type TableMeta struct {
	DBName      string
	Schema      string
	Relname     string
	Relfilenode int
	Columns     []*Column
	Relkind     string
	Hastoast    bool
	Toastrelid  int
	PrimaryKey  []string
	TypeNames   map[int]string
	RoleMap     map[int]string
}

// FullName schema.relname
func (t *TableMeta) FullName() string { return t.Schema + "." + t.Relname }
