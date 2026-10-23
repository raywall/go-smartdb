// Package catalog transforma datasets JSON em bancos/tabelas do motor SQL em
// memória (go-mysql-server) e mantém o vínculo tabela <-> arquivo para
// recarga automática (watch) e persistência das alterações (persist).
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/go-mysql-server/memory"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/dolthub/vitess/go/vt/proto/query"

	"github.com/raywall/go-dbserver-plugin/internal/source"
)

// Options controla o comportamento do catálogo.
type Options struct {
	Paths     []string // arquivos e/ou diretórios de origem
	DefaultDB string   // banco que recebe os arquivos "soltos"
	PKColumn  string   // nome da coluna usada como PK ("" desativa)
	// CaseSensitive usa collation binária (utf8mb4_0900_bin) nas colunas de
	// texto. O padrão é utf8mb4_0900_ai_ci, igual ao MySQL 8.
	CaseSensitive bool
}

// binding liga uma tabela SQL ao arquivo de onde ela veio.
type binding struct {
	db, table string
	path      string
	format    source.Format
	wrapKey   string
	modTime   time.Time
	hash      [32]byte // hash do conteúdo serializado (evita regravar sem mudança)
	schemaSig string   // assinatura do schema (detecta mudança estrutural)
}

// Catalog é o conjunto de bancos servidos.
type Catalog struct {
	opts     Options
	Provider *memory.DbProvider
	dbs      map[string]*memory.Database
	dbDirs   map[string]string // banco -> diretório (onde novas tabelas são salvas)
	bindings map[string]*binding
	mu       sync.Mutex
	log      *slog.Logger
}

// New descobre os arquivos, cria os bancos e carrega todas as tabelas.
func New(opts Options, log *slog.Logger) (*Catalog, error) {
	c := &Catalog{
		opts:     opts,
		dbs:      map[string]*memory.Database{},
		dbDirs:   map[string]string{},
		bindings: map[string]*binding{},
		log:      log,
	}
	entries, err := source.Discover(opts.Paths, opts.DefaultDB)
	if err != nil {
		return nil, err
	}
	c.resolveDirs()

	// Garante que o banco padrão sempre exista, mesmo sem arquivos.
	c.database(opts.DefaultDB)
	for _, e := range entries {
		c.database(e.Database)
	}
	var all []sql.Database
	for _, db := range c.dbs {
		all = append(all, db)
	}
	c.Provider = memory.NewDBProvider(all...)

	ctx := c.NewContext()
	for _, e := range entries {
		if err := c.loadEntry(ctx, e); err != nil {
			return nil, err
		}
	}
	if err := commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// resolveDirs descobre o diretório "dono" de cada banco, usado para gravar
// tabelas criadas via CREATE TABLE quando a persistência está ligada.
func (c *Catalog) resolveDirs() {
	for _, p := range c.opts.Paths {
		info, err := os.Stat(p)
		if err != nil || !info.IsDir() {
			continue
		}
		abs, _ := filepath.Abs(p)
		if _, ok := c.dbDirs[c.opts.DefaultDB]; !ok {
			c.dbDirs[c.opts.DefaultDB] = abs
		}
		items, _ := os.ReadDir(abs)
		for _, it := range items {
			if it.IsDir() && !strings.HasPrefix(it.Name(), ".") {
				c.dbDirs[source.Identifier(it.Name())] = filepath.Join(abs, it.Name())
			}
		}
	}
}

func (c *Catalog) database(name string) *memory.Database {
	if db, ok := c.dbs[name]; ok {
		return db
	}
	db := memory.NewDatabase(name)
	db.BaseDatabase.EnablePrimaryKeyIndexes()
	c.dbs[name] = db
	if c.Provider != nil {
		// Banco que surgiu depois da inicialização (watch).
		_ = c.Provider.CreateDatabase(c.NewContext(), name)
		if d, err := c.Provider.Database(c.NewContext(), name); err == nil {
			if md, ok := d.(*memory.Database); ok {
				c.dbs[name] = md
				return md
			}
		}
	}
	return db
}

// NewContext cria um contexto SQL interno (fora de qualquer conexão cliente).
func (c *Catalog) NewContext() *sql.Context {
	sess := memory.NewSession(sql.NewBaseSession(), c.Provider)
	return sql.NewContext(context.Background(), sql.WithSession(sess))
}

func key(db, table string) string { return strings.ToLower(db + "." + table) }

// sqlType mapeia o tipo inferido para um tipo MySQL.
func sqlType(k source.Kind, coll sql.CollationID) sql.Type {
	switch k {
	case source.KindBool:
		return types.Boolean
	case source.KindInt:
		return types.Int64
	case source.KindFloat:
		return types.Float64
	case source.KindJSON:
		return types.JSON
	default:
		return types.CreateLongText(coll)
	}
}

func (c *Catalog) schemaFor(ds *source.Dataset) sql.PrimaryKeySchema {
	coll := sql.Collation_utf8mb4_0900_ai_ci
	if c.opts.CaseSensitive {
		coll = sql.Collation_utf8mb4_0900_bin
	}
	schema := make(sql.Schema, 0, len(ds.Columns))
	for _, col := range ds.Columns {
		t := sqlType(col.Kind, coll)
		if col.PrimaryKey && col.Kind == source.KindString {
			// PK textual precisa de tamanho definido para indexação.
			t = types.MustCreateString(query.Type_VARCHAR, 255, coll)
		}
		schema = append(schema, &sql.Column{
			Name:       col.Name,
			Type:       t,
			Nullable:   col.Nullable && !col.PrimaryKey,
			PrimaryKey: col.PrimaryKey,
			Source:     ds.Table,
		})
	}
	return sql.NewPrimaryKeySchema(schema)
}

func schemaSignature(ds *source.Dataset) string {
	var b strings.Builder
	for _, col := range ds.Columns {
		fmt.Fprintf(&b, "%s:%d:%v:%v;", col.Name, col.Kind, col.Nullable, col.PrimaryKey)
	}
	return b.String()
}

// toSQLValue converte um valor decodificado do JSON para o valor SQL.
func toSQLValue(v any, k source.Kind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case source.KindBool:
		if b, ok := v.(bool); ok {
			if b {
				return int8(1), nil
			}
			return int8(0), nil
		}
	case source.KindInt:
		if n, ok := v.(json.Number); ok {
			return n.Int64()
		}
	case source.KindFloat:
		if n, ok := v.(json.Number); ok {
			return n.Float64()
		}
	case source.KindJSON:
		return types.JSONDocument{Val: normalizeNumbers(v)}, nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	// Fallback: representa o valor como texto JSON.
	b, err := json.Marshal(v)
	return string(b), err
}

// normalizeNumbers converte json.Number em float64/int64 dentro de valores
// aninhados, que é o formato esperado pelo tipo JSON do motor.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return float64(i)
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = normalizeNumbers(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalizeNumbers(x)
		}
		return out
	default:
		return v
	}
}

func (c *Catalog) loadEntry(ctx *sql.Context, e source.Entry) error {
	ds, err := source.Load(e, c.opts.PKColumn)
	if err != nil {
		return err
	}
	return c.install(ctx, ds)
}

// install cria (ou recria) a tabela a partir do dataset.
func (c *Catalog) install(ctx *sql.Context, ds *source.Dataset) error {
	db := c.database(ds.Database)
	sig := schemaSignature(ds)
	k := key(ds.Database, ds.Table)

	var tbl *memory.Table
	if prev, ok := c.bindings[k]; ok && prev.schemaSig == sig {
		// Mesmo schema: reaproveita a tabela e troca apenas os dados.
		t, _, _ := db.GetTableInsensitive(ctx, ds.Table)
		if mt, ok := t.(*memory.Table); ok {
			if _, err := mt.Truncate(ctx); err != nil {
				return err
			}
			tbl = mt
		}
	}
	if tbl == nil {
		if _, ok := c.bindings[k]; ok {
			_ = db.DropTable(ctx, ds.Table)
		}
		tbl = memory.NewTable(db, ds.Table, c.schemaFor(ds), db.GetForeignKeyCollection())
		tbl.EnablePrimaryKeyIndexes() // expõe a PK como índice (SHOW INDEX / DBeaver)
		db.AddTable(ds.Table, tbl)
	}

	inserter := tbl.Inserter(ctx)
	for i, r := range ds.Rows {
		row := make(sql.Row, len(ds.Columns))
		for j, col := range ds.Columns {
			v, err := toSQLValue(r[col.Name], col.Kind)
			if err != nil {
				return fmt.Errorf("%s linha %d coluna %q: %w", ds.Path, i+1, col.Name, err)
			}
			row[j] = v
		}
		if err := inserter.Insert(ctx, row); err != nil {
			_ = inserter.Close(ctx)
			return fmt.Errorf("%s linha %d: %w", ds.Path, i+1, err)
		}
	}
	if err := inserter.Close(ctx); err != nil {
		return err
	}

	b := &binding{
		db: ds.Database, table: ds.Table, path: ds.Path,
		format: ds.Format, wrapKey: ds.WrapKey, modTime: ds.ModTime, schemaSig: sig,
	}
	// Hash inicial calculado a partir da própria tabela, para que a
	// persistência não reescreva arquivos que não foram alterados.
	if data, err := c.serialize(ctx, b); err == nil {
		b.hash = sha256.Sum256(data)
	}
	c.bindings[k] = b

	c.log.Info("tabela carregada",
		"database", ds.Database, "table", ds.Table,
		"rows", len(ds.Rows), "columns", len(ds.Columns), "file", ds.Path)
	return nil
}

// Describe devolve um resumo textual das tabelas carregadas.
func (c *Catalog) Describe() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, b := range c.bindings {
		out = append(out, fmt.Sprintf("%s.%s  <-  %s", b.db, b.table, b.path))
	}
	return out
}
