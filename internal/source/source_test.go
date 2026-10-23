package source

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadInferSchema(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "agentes.json", `[
		{"nome":"Ana","id":1,"ativo":true,"nota":7,"tags":["a"]},
		{"nome":"Bia","id":2,"ativo":false,"nota":8.5,"extra":"x"}
	]`)
	ds, err := Load(Entry{Database: "db", Table: "agentes", Path: p}, "id")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		kind Kind
		null bool
		pk   bool
	}{
		{"id", KindInt, false, true}, // PK vai para o início
		{"nome", KindString, false, false},
		{"ativo", KindBool, false, false},
		{"nota", KindFloat, false, false}, // int + float = DOUBLE
		{"tags", KindJSON, true, false},
		{"extra", KindString, true, false},
	}
	if len(ds.Columns) != len(want) {
		t.Fatalf("colunas = %+v", ds.Columns)
	}
	for i, w := range want {
		c := ds.Columns[i]
		if c.Name != w.name || c.Kind != w.kind || c.Nullable != w.null || c.PrimaryKey != w.pk {
			t.Errorf("coluna %d = %+v, esperado %+v", i, c, w)
		}
	}
}

func TestFormats(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		file, body string
		format     Format
		rows       int
	}{
		"array":   {"a.json", `[{"x":1},{"x":2}]`, FormatArray, 2},
		"wrapped": {"b.json", `{"itens":[{"x":1}]}`, FormatWrapped, 1},
		"object":  {"c.json", `{"x":1,"y":2}`, FormatObject, 1},
		"ndjson":  {"d.ndjson", "{\"x\":1}\n\n{\"x\":2}\n", FormatNDJSON, 2},
		"scalars": {"e.json", `["a","b","c"]`, FormatArray, 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := write(t, dir, tc.file, tc.body)
			ds, err := Load(Entry{Path: p}, "id")
			if err != nil {
				t.Fatal(err)
			}
			if ds.Format != tc.format || len(ds.Rows) != tc.rows {
				t.Fatalf("format=%v rows=%d", ds.Format, len(ds.Rows))
			}
		})
	}
}

func TestDuplicateIDIsNotPK(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "x.json", `[{"id":1},{"id":1}]`)
	ds, err := Load(Entry{Path: p}, "id")
	if err != nil {
		t.Fatal(err)
	}
	if ds.Columns[0].PrimaryKey {
		t.Fatal("id duplicado não deveria virar PK")
	}
}

func TestDiscoverSubdirsBecomeDatabases(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "agentes.json", `[]`)
	write(t, dir, "rh/cargos.json", `[]`)
	write(t, dir, "ignorar.txt", `x`)
	got, err := Discover([]string{dir}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Database != "main" || got[1].Database != "rh" || got[1].Table != "cargos" {
		t.Fatalf("got %+v", got)
	}
}

func TestIdentifier(t *testing.T) {
	for in, want := range map[string]string{
		"vendas 2024": "vendas_2024",
		"2024-dados":  "t_2024_dados",
		"ação!":       "ação",
	} {
		if got := Identifier(in); got != want {
			t.Errorf("Identifier(%q) = %q, esperado %q", in, got, want)
		}
	}
}
