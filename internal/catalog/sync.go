package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dolthub/go-mysql-server/memory"
	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"

	"github.com/raywall/go-smartdb/internal/source"
)

// commit efetiva no banco global as alterações feitas pela sessão interna.
func commit(ctx *sql.Context) error {
	if s, ok := ctx.Session.(*memory.Session); ok {
		return s.CommitTransaction(ctx, nil)
	}
	return nil
}

// Run executa o laço de sincronização até o contexto ser cancelado.
//   - watch:   recarrega tabelas quando o arquivo muda no disco e carrega
//     arquivos novos que surgirem nos diretórios observados;
//   - persist: grava de volta no formato de origem as alterações feitas via SQL.
func (c *Catalog) Run(ctx context.Context, interval time.Duration, watch, persist bool) {
	if !watch && !persist {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if persist {
				c.Flush() // última gravação antes de encerrar
			}
			return
		case <-t.C:
			if watch {
				c.reloadChanged()
			}
			if persist {
				c.Flush()
			}
		}
	}
}

// reloadChanged recarrega tabelas cujos arquivos mudaram e carrega novos.
func (c *Catalog) reloadChanged() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := c.NewContext()

	for k, b := range c.bindings {
		info, err := os.Stat(b.path)
		if os.IsNotExist(err) {
			if db, ok := c.dbs[b.db]; ok {
				_ = db.DropTable(ctx, b.table)
			}
			delete(c.bindings, k)
			c.log.Info("arquivo removido; tabela descartada", "table", k, "file", b.path)
			continue
		}
		if err != nil || info.ModTime().Equal(b.modTime) {
			continue
		}
		e := source.Entry{Database: b.db, Table: b.table, Path: b.path}
		if err := c.loadEntry(ctx, e); err != nil {
			// Arquivo pode estar no meio de uma gravação; tenta no próximo ciclo
			// mantendo os dados atuais.
			c.log.Warn("falha ao recarregar; mantendo dados anteriores", "file", b.path, "err", err)
			b.modTime = info.ModTime()
			continue
		}
		c.log.Info("tabela recarregada do disco", "table", k)
	}

	entries, err := source.Discover(c.opts.Paths, c.opts.DefaultDB)
	if err != nil {
		c.log.Warn("falha ao varrer diretórios", "err", err)
		return
	}
	for _, e := range entries {
		if _, ok := c.bindings[key(e.Database, e.Table)]; ok {
			continue
		}
		if err := c.loadEntry(ctx, e); err != nil {
			c.log.Warn("falha ao carregar novo arquivo", "file", e.Path, "err", err)
		}
	}
	if err := commit(ctx); err != nil {
		c.log.Warn("falha ao efetivar recarga", "err", err)
	}
}

// Flush grava em disco as tabelas que mudaram desde a última leitura/gravação.
// Tabelas criadas via CREATE TABLE em um banco ligado a um diretório ganham
// um novo arquivo <tabela>.json nesse diretório.
func (c *Catalog) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx := c.NewContext()

	for name, db := range c.dbs {
		names, err := db.GetTableNames(ctx)
		if err != nil {
			continue
		}
		present := map[string]bool{}
		for _, tn := range names {
			k := key(name, tn)
			present[k] = true
			if _, ok := c.bindings[k]; ok {
				continue
			}
			dir, ok := c.dbDirs[name]
			if !ok {
				continue // banco sem diretório: tabela fica só em memória
			}
			c.bindings[k] = &binding{
				db: name, table: tn, format: source.FormatArray,
				path: filepath.Join(dir, tn+".json"),
			}
			c.log.Info("nova tabela será persistida", "table", k, "file", c.bindings[k].path)
		}
		// DROP TABLE não apaga o arquivo (por segurança); apenas desvincula.
		for k, b := range c.bindings {
			if b.db == name && !present[k] {
				delete(c.bindings, k)
				c.log.Warn("tabela removida via SQL; arquivo mantido em disco", "table", k, "file", b.path)
			}
		}
	}

	for k, b := range c.bindings {
		data, err := c.serialize(ctx, b)
		if err != nil {
			c.log.Warn("falha ao serializar tabela", "table", k, "err", err)
			continue
		}
		h := sha256.Sum256(data)
		if h == b.hash {
			continue
		}
		if err := writeAtomic(b.path, data); err != nil {
			c.log.Error("falha ao gravar arquivo", "file", b.path, "err", err)
			continue
		}
		b.hash = h
		if info, err := os.Stat(b.path); err == nil {
			b.modTime = info.ModTime()
		}
		c.log.Info("alterações gravadas", "table", k, "file", b.path)
	}
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".smartdb-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// serialize lê a tabela atual e gera o conteúdo do arquivo no formato original.
func (c *Catalog) serialize(ctx *sql.Context, b *binding) ([]byte, error) {
	db, ok := c.dbs[b.db]
	if !ok {
		return nil, fmt.Errorf("banco %q não encontrado", b.db)
	}
	t, ok, err := db.GetTableInsensitive(ctx, b.table)
	if err != nil || !ok {
		return nil, fmt.Errorf("tabela %q não encontrada", b.table)
	}
	schema := t.Schema()

	var objects [][]byte
	parts, err := t.Partitions(ctx)
	if err != nil {
		return nil, err
	}
	defer parts.Close(ctx)
	for {
		p, err := parts.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		iter, err := t.PartitionRows(ctx, p)
		if err != nil {
			return nil, err
		}
		rows, err := sql.RowIterToRows(ctx, iter)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			obj, err := encodeRow(schema, r)
			if err != nil {
				return nil, err
			}
			objects = append(objects, obj)
		}
	}

	var buf bytes.Buffer
	switch b.format {
	case source.FormatCSV:
		return serializeCSV(objects, schema, b)
	case source.FormatNDJSON:
		for _, o := range objects {
			buf.Write(o)
			buf.WriteByte('\n')
		}
		return buf.Bytes(), nil
	case source.FormatObject:
		if len(objects) == 1 {
			if err := json.Indent(&buf, objects[0], "", "  "); err != nil {
				return nil, err
			}
			buf.WriteByte('\n')
			return buf.Bytes(), nil
		}
	}

	arr := []byte("[" + string(bytes.Join(objects, []byte(","))) + "]")
	if b.format == source.FormatWrapped || b.format == source.FormatYAMLWrapped {
		k, _ := json.Marshal(b.wrapKey)
		arr = []byte("{" + string(k) + ":" + string(arr) + "}")
	}
	if b.format >= source.FormatYAMLArray && b.format <= source.FormatYAMLDocuments {
		return serializeYAML(objects, arr, b.format)
	}
	if err := json.Indent(&buf, arr, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// encodeRow monta um objeto JSON preservando a ordem das colunas.
// Valores NULL são omitidos (equivalente a "chave ausente" no arquivo).
func encodeRow(schema sql.Schema, r sql.Row) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for i, col := range schema {
		if r[i] == nil {
			continue
		}
		v, err := jsonValue(col, r[i])
		if err != nil {
			return nil, fmt.Errorf("coluna %q: %w", col.Name, err)
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(col.Name)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func jsonValue(col *sql.Column, v any) (any, error) {
	if w, ok := v.(sql.JSONWrapper); ok {
		return w.ToInterface()
	}
	// BOOLEAN no MySQL é TINYINT(1): devolve true/false no JSON.
	if types.IsBoolean(col.Type) || strings.EqualFold(col.Type.String(), "tinyint(1)") {
		switch n := v.(type) {
		case int8:
			return n != 0, nil
		case bool:
			return n, nil
		}
	}
	switch t := v.(type) {
	case time.Time:
		return t.Format(time.RFC3339Nano), nil
	case decimal.Decimal:
		return json.Number(t.String()), nil
	case []byte:
		return string(t), nil
	}
	return v, nil
}

// serializeCSV mantém a ordem original do cabeçalho, mesmo quando a PK foi
// movida para o início do schema SQL. Colunas novas são acrescentadas ao final.
func serializeCSV(objects [][]byte, schema sql.Schema, b *binding) ([]byte, error) {
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	writer.Comma = b.csv.Delimiter
	writer.UseCRLF = b.csv.UseCRLF
	var headers []string
	present := map[string]bool{}
	for _, col := range schema {
		present[col.Name] = true
	}
	for _, name := range b.headers {
		if present[name] {
			headers = append(headers, name)
			delete(present, name)
		}
	}
	for _, col := range schema {
		if present[col.Name] {
			headers = append(headers, col.Name)
		}
	}
	if !b.csv.NoHeader {
		if err := writer.Write(headers); err != nil {
			return nil, err
		}
	}
	for _, object := range objects {
		var values map[string]any
		decoder := json.NewDecoder(bytes.NewReader(object))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			return nil, err
		}
		record := make([]string, len(headers))
		for i, name := range headers {
			switch value := values[name].(type) {
			case nil:
			case string:
				record[i] = value
			default:
				encoded, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				record[i] = string(encoded)
			}
		}
		if err := writer.Write(record); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return buf.Bytes(), writer.Error()
}

func serializeYAML(objects [][]byte, array []byte, format source.Format) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	encode := func(data []byte) error {
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			return err
		}
		var blockStyle func(*yaml.Node)
		blockStyle = func(n *yaml.Node) {
			n.Style = 0
			for _, child := range n.Content {
				blockStyle(child)
			}
		}
		blockStyle(&node)
		return encoder.Encode(&node)
	}
	if format == source.FormatYAMLDocuments && len(objects) > 0 {
		for _, object := range objects {
			if err := encode(object); err != nil {
				return nil, err
			}
		}
	} else {
		if format == source.FormatYAMLObject && len(objects) == 1 {
			array = objects[0]
		}
		if err := encode(array); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
