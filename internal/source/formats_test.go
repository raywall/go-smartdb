// source_test verifies file formats, schema inference and discovery in go-smartdb.
// Author: go-smartdb maintainers. Created: 2026-10-23.
package source_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/raywall/go-smartdb/internal/source"
)

func load(t *testing.T, name, body string, opts source.CSVOptions) *source.Dataset {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	ds, err := source.LoadWithOptions(source.Entry{Path: path}, "id", opts)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// TestCSV verifies common dialects, quoting, nulls, and conservative column inference.
func TestCSV(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		opts       source.CSVOptions
		delimiter  rune
		columns    []source.Column
		rows       []map[string]any
	}{
		{"comma", "nome,id,nota,ativo,codigo,extra\nAna,1,7,true,00123,\nBia,2,8.5,false,00124,x\n", source.CSVOptions{}, ',',
			[]source.Column{{Name: "id", Kind: source.KindInt, PrimaryKey: true}, {Name: "nome", Kind: source.KindString}, {Name: "nota", Kind: source.KindFloat}, {Name: "ativo", Kind: source.KindBool}, {Name: "codigo", Kind: source.KindString}, {Name: "extra", Kind: source.KindString, Nullable: true}},
			[]map[string]any{{"nome": "Ana", "id": json.Number("1"), "nota": json.Number("7"), "ativo": true, "codigo": "00123", "extra": nil}, {"nome": "Bia", "id": json.Number("2"), "nota": json.Number("8.5"), "ativo": false, "codigo": "00124", "extra": "x"}}},
		{"semicolon", "\ufeffid;texto\r\n1;\"olá; mundo\r\nnova linha\"\r\n", source.CSVOptions{}, ';', nil, []map[string]any{{"id": json.Number("1"), "texto": "olá; mundo\nnova linha"}}},
		{"header_punctuation", "id;address,city\n1;Recife\n", source.CSVOptions{}, ';', nil, []map[string]any{{"id": json.Number("1"), "address,city": "Recife"}}},
		{"tab", "id\tnome\n1\tAna\n", source.CSVOptions{}, '\t', nil, []map[string]any{{"id": json.Number("1"), "nome": "Ana"}}},
		{"pipe", "id|nome\n1|Ana\n", source.CSVOptions{}, '|', nil, []map[string]any{{"id": json.Number("1"), "nome": "Ana"}}},
		{"custom_no_header", "1:Ana\n2:Bia\n", source.CSVOptions{Delimiter: ':', NoHeader: true}, ':', nil, []map[string]any{{"column_1": json.Number("1"), "column_2": "Ana"}, {"column_1": json.Number("2"), "column_2": "Bia"}}},
		{"mixed_text", "id,value\n1,10\n2,N/A\n", source.CSVOptions{}, ',', nil, []map[string]any{{"id": json.Number("1"), "value": "10"}, {"id": json.Number("2"), "value": "N/A"}}},
		{"header_only", "id,nome\n", source.CSVOptions{}, ',', []source.Column{{Name: "id", Kind: source.KindString, PrimaryKey: true}, {Name: "nome", Kind: source.KindString}}, []map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ds := load(t, "dados.csv", tc.body, tc.opts)
			if ds.Format != source.FormatCSV || ds.CSV.Delimiter != tc.delimiter {
				t.Fatalf("format/dialect: %+v", ds)
			}
			if !reflect.DeepEqual(ds.Rows, tc.rows) {
				t.Fatalf("rows = %#v; want %#v", ds.Rows, tc.rows)
			}
			if tc.columns != nil && !reflect.DeepEqual(ds.Columns, tc.columns) {
				t.Fatalf("columns = %+v; want %+v", ds.Columns, tc.columns)
			}
		})
	}
}

// TestYAML verifies shapes, aliases, ordering and values shared with JSON.
func TestYAML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		format     source.Format
		rows       int
		wrap       string
	}{
		{"array", "- nome: Ana\n  id: 1\n  nota: 8.5\n  ativo: true\n  tags: [a, b]\n- nome: Bia\n  id: 2\n", source.FormatYAMLArray, 2, ""},
		{"wrapped", "pessoas:\n  - id: 1\n    nome: Ana\n", source.FormatYAMLWrapped, 1, "pessoas"},
		{"object", "id: 1\nnome: Ana\n", source.FormatYAMLObject, 1, ""},
		{"documents", "id: 1\n---\nid: 2\n", source.FormatYAMLDocuments, 2, ""},
		{"document_arrays", "- id: 1\n---\n- id: 2\n", source.FormatYAMLDocuments, 2, ""},
		{"scalars", "- a\n- b\n", source.FormatYAMLArray, 2, ""},
		{"scalar", "hello\n", source.FormatYAMLObject, 1, ""},
		{"alias", "- &base\n  id: 1\n  nome: Ana\n- <<: *base\n  id: 2\n", source.FormatYAMLArray, 2, ""},
		{"empty", "", source.FormatYAMLArray, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ds := load(t, "dados.YML", tc.body, source.CSVOptions{})
			if ds.Format != tc.format || len(ds.Rows) != tc.rows || ds.WrapKey != tc.wrap {
				t.Fatalf("dataset: %+v", ds)
			}
			if tc.name == "array" {
				want := []source.Column{{Name: "id", Kind: source.KindInt, PrimaryKey: true}, {Name: "nome", Kind: source.KindString}, {Name: "nota", Kind: source.KindFloat, Nullable: true}, {Name: "ativo", Kind: source.KindBool, Nullable: true}, {Name: "tags", Kind: source.KindJSON, Nullable: true}}
				if !reflect.DeepEqual(ds.Columns, want) {
					t.Fatalf("columns = %+v", ds.Columns)
				}
				if ds.Rows[0]["id"] != json.Number("1") {
					t.Fatalf("integer lost: %#v", ds.Rows[0])
				}
			}
			if tc.name == "alias" && ds.Rows[1]["nome"] != "Ana" {
				t.Fatal("alias not resolved")
			}
		})
	}
}

// TestInvalidFiles verifies that malformed input fails with source context.
func TestInvalidFiles(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, body string }{
		{"duplicate.csv", "id,ID\n1,2\n"}, {"empty_header.csv", "id,\n1,2\n"},
		{"ragged.csv", "id,nome\n1\n"}, {"quote.csv", "id,nome\n1,\"broken\n"},
		{"encoding.csv", "id,nome\n1,\xff\n"}, {"broken.yaml", "items: [broken\n"},
		{"keys.yml", "1: numeric key\n"}, {"nan.yaml", "value: .nan\n"},
		{"duplicate.yaml", "id: 1\nid: 2\n"}, {"unsupported.txt", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := source.Load(source.Entry{Path: path}, "id")
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected contextual error, got %v", err)
			}
		})
	}
}

// TestDiscoverMixedFormats verifies directory databases and cross-format collisions.
func TestDiscoverMixedFormats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "rh"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.csv", "rh/c.YAML", "rh/d.yml", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := source.Discover([]string{root}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 || entries[2].Database != "rh" || entries[2].Table != "c" {
		t.Fatalf("entries: %+v", entries)
	}
	if err := os.WriteFile(filepath.Join(root, "a.csv"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Discover([]string{root}, "main"); err == nil {
		t.Fatal("expected cross-format collision")
	}
}
