// catalog verifies SQL joins, persistence and reload behavior across file formats.
// Author: go-smartdb maintainers. Created: 2026-10-23.
// Os testes deste arquivo são sequenciais porque sqle.NewDefault altera
// variáveis globais do motor SQL durante a inicialização.
package catalog

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/raywall/go-smartdb/internal/source"
)

func fixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newCatalog(t *testing.T, dir string) *Catalog {
	t.Helper()
	c, err := New(Options{Paths: []string{dir}, DefaultDB: "main", PKColumn: "id"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func executeQuery(t *testing.T, c *Catalog, statement string) []sql.Row {
	t.Helper()
	engine := sqle.NewDefault(c.Provider)
	defer engine.Close()
	ctx := c.NewContext()
	_, iter, _, err := engine.Query(ctx, statement)
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	rows, err := sql.RowIterToRows(ctx, iter)
	if err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
	if err := commit(ctx); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestMixedFormatJoin exercises the SQL engine with three file formats and databases.
func TestMixedFormatJoin(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "agentes.json", `[{"id":1,"nome":"Ana","departamento_id":10},{"id":2,"nome":"Bia","departamento_id":20}]`)
	fixture(t, dir, "vendas.csv", "id,agente_id,valor\n1,1,12.5\n2,1,7.5\n3,2,8\n")
	fixture(t, dir, "rh/departamentos.yaml", "- id: 10\n  nome: Engenharia\n- id: 20\n  nome: Comercial\n")
	c := newCatalog(t, dir)
	got := executeQuery(t, c, `SELECT a.nome, d.nome, SUM(v.valor) FROM main.agentes a JOIN main.vendas v ON v.agente_id=a.id JOIN rh.departamentos d ON d.id=a.departamento_id GROUP BY a.nome, d.nome ORDER BY a.nome`)
	want := []sql.Row{{"Ana", "Engenharia", float64(20)}, {"Bia", "Comercial", float64(8)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %#v; want %#v", got, want)
	}
}

// TestPersistFormats verifies that SQL changes retain the source encoding and dialect.
func TestPersistFormats(t *testing.T) {
	cases := []struct {
		name, body string
		format     source.Format
		opts       source.CSVOptions
	}{
		{"people.csv", "nome;id;ativo\r\nAna;1;true\r\n", source.FormatCSV, source.CSVOptions{}},
		{"people.yaml", "- nome: Ana\n  id: 1\n  ativo: true\n", source.FormatYAMLArray, source.CSVOptions{}},
		{"people.yml", "people:\n  - nome: Ana\n    id: 1\n    ativo: true\n", source.FormatYAMLWrapped, source.CSVOptions{}},
		{"object.yaml", "nome: Ana\nid: 1\nativo: true\n", source.FormatYAMLObject, source.CSVOptions{}},
		{"documents.yaml", "nome: Ana\nid: 1\nativo: true\n---\nnome: Bia\nid: 2\nativo: false\n", source.FormatYAMLDocuments, source.CSVOptions{}},
		{"noheader.csv", "Ana|1|true\n", source.FormatCSV, source.CSVOptions{NoHeader: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := fixture(t, dir, tc.name, tc.body)
			c, err := New(Options{Paths: []string{dir}, DefaultDB: "main", PKColumn: "id", CSV: tc.opts}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			c.Flush()
			unchanged, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(unchanged) != tc.body {
				t.Fatal("unchanged source was rewritten")
			}
			table := source.TableName(path)
			column := "nome"
			if tc.opts.NoHeader {
				column = "column_1"
			}
			executeQuery(t, c, "UPDATE main."+table+" SET "+column+" = 'Atualizado'")
			c.Flush()
			ds, err := source.LoadWithOptions(source.Entry{Path: path}, "id", tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if ds.Format != tc.format {
				t.Fatalf("format = %v; want %v", ds.Format, tc.format)
			}
			if len(ds.Rows) == 0 || ds.Rows[0][column] != "Atualizado" {
				t.Fatalf("persisted rows: %#v", ds.Rows)
			}
			if tc.format == source.FormatCSV && !tc.opts.NoHeader {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(data, []byte("nome;id;ativo\r\n")) {
					t.Fatalf("dialect/header lost: %q", data)
				}
			}
			if tc.opts.NoHeader {
				return
			}
			reopened := newCatalog(t, dir)
			rows := executeQuery(t, reopened, "SELECT "+column+" FROM main."+table)
			if len(rows) != len(ds.Rows) || rows[0][0] != "Atualizado" {
				t.Fatalf("reopened rows: %#v", rows)
			}
		})
	}
}

// TestReloadFormats verifies changed files and newly discovered CSV/YAML tables.
func TestReloadFormats(t *testing.T) {
	dir := t.TempDir()
	path := fixture(t, dir, "people.csv", "id,nome\n1,Ana\n")
	c := newCatalog(t, dir)
	fixture(t, dir, "people.csv", "id,nome\n1,Atualizado\n")
	// Define mtime explicitamente para não depender da resolução do filesystem.
	mtime := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	fixture(t, dir, "rh/teams.yml", "- id: 1\n  nome: Engenharia\n")
	c.reloadChanged()
	got := executeQuery(t, c, "SELECT p.nome, t.nome FROM main.people p JOIN rh.teams t ON t.id=p.id")
	if !reflect.DeepEqual(got, []sql.Row{{"Atualizado", "Engenharia"}}) {
		t.Fatalf("rows: %#v", got)
	}
	yamlPath := fixture(t, dir, "rh/teams.yml", "- id: 1\n  nome: Produto\n")
	if err := os.Chtimes(yamlPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	fixture(t, dir, "new.csv", "id,value\n1,novo\n")
	c.reloadChanged()
	got = executeQuery(t, c, "SELECT t.nome, n.value FROM rh.teams t JOIN main.new n ON n.id=t.id")
	if !reflect.DeepEqual(got, []sql.Row{{"Produto", "novo"}}) {
		t.Fatalf("rows: %#v", got)
	}
}
