// Package source descobre arquivos JSON no disco e os converte em
// "datasets" tabulares (colunas + linhas) prontos para virar tabelas SQL.
package source

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Format indica como o arquivo de origem está estruturado. É usado tanto na
// leitura quanto na escrita (persistência), para preservar o formato original.
type Format int

const (
	// FormatArray: o arquivo é um array JSON de objetos: [ {...}, {...} ].
	FormatArray Format = iota
	// FormatWrapped: o arquivo é um objeto com uma única chave contendo o
	// array: { "agentes": [ {...} ] }. WrapKey guarda o nome da chave.
	FormatWrapped
	// FormatNDJSON: um objeto JSON por linha (.ndjson / .jsonl).
	FormatNDJSON
	// FormatObject: o arquivo é um único objeto; vira uma tabela de 1 linha.
	FormatObject
)

// Dataset é o resultado da leitura de um arquivo JSON.
type Dataset struct {
	Database string    // nome do banco (diretório) ao qual a tabela pertence
	Table    string    // nome da tabela (derivado do nome do arquivo)
	Path     string    // caminho absoluto do arquivo
	Format   Format    // formato original do arquivo
	WrapKey  string    // chave do objeto envelope (FormatWrapped)
	ModTime  time.Time // mtime no momento da leitura
	Columns  []Column  // colunas inferidas, na ordem de primeira aparição
	Rows     []map[string]any
	keyOrder []string // ordem das chaves conforme aparecem no arquivo
}

// Kind é o tipo lógico inferido para uma coluna.
type Kind int

const (
	KindNull Kind = iota // apenas nulos (vira TEXT)
	KindBool
	KindInt
	KindFloat
	KindString
	KindJSON // objetos, arrays ou mistura de tipos incompatíveis
)

func (k Kind) String() string {
	return [...]string{"NULL", "BOOLEAN", "BIGINT", "DOUBLE", "TEXT", "JSON"}[k]
}

// Column descreve uma coluna inferida.
type Column struct {
	Name       string
	Kind       Kind
	Nullable   bool
	PrimaryKey bool
}

var supportedExt = map[string]bool{".json": true, ".ndjson": true, ".jsonl": true}

// IsSupported informa se a extensão do arquivo é reconhecida.
func IsSupported(path string) bool {
	return supportedExt[strings.ToLower(filepath.Ext(path))]
}

// Entry associa um arquivo a um banco/tabela.
type Entry struct {
	Database string
	Table    string
	Path     string
}

// Discover percorre os caminhos informados e devolve os arquivos JSON
// encontrados. Regras:
//   - arquivo avulso       -> tabela no banco padrão
//   - arquivos de um dir.  -> tabelas no banco padrão
//   - subdiretórios        -> cada subdiretório vira um banco com o seu nome
func Discover(paths []string, defaultDB string) ([]Entry, error) {
	var out []Entry
	seen := map[string]string{} // db.table -> path (detecta colisões)

	add := func(db, path string) error {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		e := Entry{Database: db, Table: TableName(path), Path: abs}
		k := strings.ToLower(e.Database + "." + e.Table)
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("tabela %q duplicada: %s e %s", k, prev, abs)
		}
		seen[k] = abs
		out = append(out, e)
		return nil
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := add(defaultDB, p); err != nil {
				return nil, err
			}
			continue
		}
		items, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			full := filepath.Join(p, it.Name())
			if strings.HasPrefix(it.Name(), ".") {
				continue
			}
			if it.IsDir() {
				sub, err := os.ReadDir(full)
				if err != nil {
					return nil, err
				}
				db := Identifier(it.Name())
				for _, s := range sub {
					if !s.IsDir() && IsSupported(s.Name()) && !strings.HasPrefix(s.Name(), ".") {
						if err := add(db, filepath.Join(full, s.Name())); err != nil {
							return nil, err
						}
					}
				}
				continue
			}
			if IsSupported(it.Name()) {
				if err := add(defaultDB, full); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Table < out[j].Table
	})
	return out, nil
}

var nonIdent = regexp.MustCompile(`[^\p{L}\p{N}_]+`)

// Identifier normaliza um texto para um identificador SQL seguro.
func Identifier(s string) string {
	s = nonIdent.ReplaceAllString(strings.TrimSpace(s), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "t"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "t_" + s
	}
	return s
}

// TableName deriva o nome da tabela a partir do nome do arquivo.
func TableName(path string) string {
	base := filepath.Base(path)
	return Identifier(strings.TrimSuffix(base, filepath.Ext(base)))
}

// Load lê e interpreta um arquivo JSON. pkName é o nome (case-insensitive)
// da coluna candidata a chave primária; string vazia desativa a detecção.
func Load(e Entry, pkName string) (*Dataset, error) {
	info, err := os.Stat(e.Path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(e.Path)
	if err != nil {
		return nil, err
	}
	ds := &Dataset{Database: e.Database, Table: e.Table, Path: e.Path, ModTime: info.ModTime()}

	ext := strings.ToLower(filepath.Ext(e.Path))
	if ext == ".ndjson" || ext == ".jsonl" {
		ds.Format = FormatNDJSON
		ds.Rows, err = parseNDJSON(raw)
	} else {
		ds.Rows, ds.Format, ds.WrapKey, err = parseJSON(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.Path, err)
	}
	ds.keyOrder = keyOrder(raw, ds.Format, ds.WrapKey)
	ds.Columns = inferColumns(ds.Rows, ds.keyOrder, pkName)
	return ds, nil
}

func decode(r io.Reader) *json.Decoder {
	d := json.NewDecoder(r)
	d.UseNumber() // preserva inteiros grandes sem perda de precisão
	return d
}

func parseJSON(raw []byte) ([]map[string]any, Format, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, FormatArray, "", nil
	}
	var v any
	if err := decode(bytes.NewReader(trimmed)).Decode(&v); err != nil {
		return nil, 0, "", err
	}
	switch t := v.(type) {
	case []any:
		rows, err := toRows(t)
		return rows, FormatArray, "", err
	case map[string]any:
		// { "chave": [ ... ] } com uma única chave do tipo array -> envelope
		if len(t) == 1 {
			for k, inner := range t {
				if arr, ok := inner.([]any); ok {
					rows, err := toRows(arr)
					return rows, FormatWrapped, k, err
				}
			}
		}
		return []map[string]any{t}, FormatObject, "", nil
	default:
		return nil, 0, "", fmt.Errorf("conteúdo raiz deve ser array ou objeto, recebido %T", v)
	}
}

func parseNDJSON(raw []byte) ([]map[string]any, error) {
	var rows []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var v any
		if err := decode(bytes.NewReader(b)).Decode(&v); err != nil {
			return nil, fmt.Errorf("linha %d: %w", line, err)
		}
		rows = append(rows, asObject(v))
	}
	return rows, sc.Err()
}

func toRows(arr []any) ([]map[string]any, error) {
	rows := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		rows = append(rows, asObject(it))
	}
	return rows, nil
}

// asObject garante que cada item seja um objeto. Valores escalares em um
// array (ex.: ["a","b"]) viram linhas com uma única coluna "value".
func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": v}
}

// kindOf classifica um valor decodificado.
func kindOf(v any) Kind {
	switch t := v.(type) {
	case nil:
		return KindNull
	case bool:
		return KindBool
	case json.Number:
		if _, err := t.Int64(); err == nil && !strings.ContainsAny(t.String(), ".eE") {
			return KindInt
		}
		return KindFloat
	case string:
		return KindString
	default:
		return KindJSON
	}
}

// merge combina dois tipos observados na mesma coluna.
func merge(a, b Kind) Kind {
	switch {
	case a == b:
		return a
	case a == KindNull:
		return b
	case b == KindNull:
		return a
	case (a == KindInt && b == KindFloat) || (a == KindFloat && b == KindInt):
		return KindFloat
	default:
		return KindJSON // tipos incompatíveis: guarda como JSON, sem perder dado
	}
}

func inferColumns(rows []map[string]any, order []string, pkName string) []Column {
	var cols []Column
	index := map[string]int{}

	// Primeiro registra as colunas na ordem em que aparecem no arquivo.
	for _, k := range order {
		index[k] = len(cols)
		cols = append(cols, Column{Name: k, Kind: KindNull})
	}
	for _, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys) // fallback determinístico para chaves não ordenadas
		for _, k := range keys {
			i, ok := index[k]
			if !ok {
				i = len(cols)
				index[k] = i
				cols = append(cols, Column{Name: k, Kind: KindNull})
			}
			kd := kindOf(r[k])
			if kd == KindNull {
				cols[i].Nullable = true
			}
			cols[i].Kind = merge(cols[i].Kind, kd)
		}
	}
	// Coluna ausente em alguma linha também é anulável.
	for i := range cols {
		for _, r := range rows {
			if _, ok := r[cols[i].Name]; !ok {
				cols[i].Nullable = true
				break
			}
		}
		if cols[i].Kind == KindNull {
			cols[i].Kind = KindString
		}
	}

	// Chave primária: coluna "id" (configurável) escalar, não nula e única.
	if pkName != "" {
		for i := range cols {
			c := &cols[i]
			if !strings.EqualFold(c.Name, pkName) || c.Nullable {
				continue
			}
			if c.Kind != KindInt && c.Kind != KindString {
				continue
			}
			uniq := map[string]bool{}
			dup := false
			for _, r := range rows {
				s := fmt.Sprint(r[c.Name])
				if uniq[s] {
					dup = true
					break
				}
				uniq[s] = true
			}
			if !dup {
				c.PrimaryKey = true
				// PK em primeiro lugar facilita a leitura no DBeaver.
				pk := *c
				cols = append([]Column{pk}, append(cols[:i:i], cols[i+1:]...)...)
			}
			break
		}
	}
	return cols
}

// keyOrder percorre o JSON bruto com o tokenizer para descobrir a ordem em
// que as chaves dos objetos aparecem (map[string]any perde essa ordem).
func keyOrder(raw []byte, f Format, wrapKey string) []string {
	var order []string
	seen := map[string]bool{}
	collect := func(obj json.RawMessage) {
		d := json.NewDecoder(bytes.NewReader(obj))
		if t, err := d.Token(); err != nil || t != json.Delim('{') {
			return
		}
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return
			}
			k, _ := t.(string)
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
			var skip json.RawMessage
			if d.Decode(&skip) != nil {
				return
			}
		}
	}
	collectArray := func(arr json.RawMessage) {
		var items []json.RawMessage
		if json.Unmarshal(arr, &items) == nil {
			for _, it := range items {
				collect(it)
			}
		}
	}
	switch f {
	case FormatArray:
		collectArray(raw)
	case FormatWrapped:
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil {
			collectArray(m[wrapKey])
		}
	case FormatObject:
		collect(raw)
	case FormatNDJSON:
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			collect(bytes.TrimSpace(sc.Bytes()))
		}
	}
	return order
}
