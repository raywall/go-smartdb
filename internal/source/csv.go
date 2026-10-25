// source implements CSV parsing and SQL-friendly type inference for go-smartdb.
// Author: go-smartdb maintainers. Created: 2026-10-23.
package source

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// CSVOptions controla o dialeto CSV. Delimiter zero detecta vírgula, ponto e
// vírgula, tabulação ou pipe. NoHeader gera nomes column_1, column_2, etc.
type CSVOptions struct {
	Delimiter rune
	NoHeader  bool
	UseCRLF   bool // preserva quebras CRLF na persistência
}

func parseCSV(raw []byte, opts CSVOptions) ([]map[string]any, []string, CSVOptions, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) {
		return nil, nil, opts, fmt.Errorf("CSV deve usar codificação UTF-8")
	}
	if opts.Delimiter == 0 {
		opts.Delimiter = detectDelimiter(raw)
	}
	opts.UseCRLF = bytes.Contains(raw, []byte("\r\n"))
	r := csv.NewReader(bytes.NewReader(raw))
	r.Comma = opts.Delimiter
	first, err := r.Read()
	if err == io.EOF {
		return nil, nil, opts, nil
	}
	if err != nil {
		return nil, nil, opts, err
	}
	headers := append([]string(nil), first...)
	seen := map[string]bool{}
	for i := range headers {
		if opts.NoHeader {
			headers[i] = fmt.Sprintf("column_%d", i+1)
		} else {
			headers[i] = strings.TrimSpace(headers[i])
		}
		k := strings.ToLower(headers[i])
		if k == "" || seen[k] {
			return nil, nil, opts, fmt.Errorf("cabeçalho CSV vazio ou duplicado na coluna %d: %q", i+1, headers[i])
		}
		seen[k] = true
	}
	var records [][]string
	if opts.NoHeader {
		records = append(records, first)
	}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, opts, err
		}
		records = append(records, row)
	}
	// Infere por coluna: uma coluna com texto mantém todos os valores como texto.
	kinds := make([]Kind, len(headers))
	for j := range headers {
		for _, row := range records {
			if row[j] == "" {
				continue
			}
			_, kind := csvScalar(row[j])
			if kinds[j] == KindNull {
				kinds[j] = kind
			} else if kinds[j] != kind {
				if (kinds[j] == KindInt && kind == KindFloat) || (kinds[j] == KindFloat && kind == KindInt) {
					kinds[j] = KindFloat
				} else {
					kinds[j] = KindString
				}
			}
		}
	}
	rows := make([]map[string]any, 0, len(records))
	for _, record := range records {
		row := make(map[string]any, len(headers))
		for j, value := range record {
			if value == "" {
				row[headers[j]] = nil
			} else if kinds[j] == KindString {
				row[headers[j]] = value
			} else {
				row[headers[j]], _ = csvScalar(value)
			}
		}
		rows = append(rows, row)
	}
	return rows, headers, opts, nil
}

func csvScalar(s string) (any, Kind) {
	if strings.EqualFold(s, "true") {
		return true, KindBool
	}
	if strings.EqualFold(s, "false") {
		return false, KindBool
	}
	// A gramática JSON evita converter códigos como 00123 em números.
	if len(s) > 0 && (s[0] == '-' || s[0] >= '0' && s[0] <= '9') && json.Valid([]byte(s)) {
		n := json.Number(s)
		if _, err := n.Float64(); err == nil {
			return n, kindOf(n)
		}
	}
	return s, KindString
}

func detectDelimiter(raw []byte) rune {
	best, width := ',', 0
	fallback, fallbackWidth := ',', 0
	for _, candidate := range []rune{',', ';', '\t', '|'} {
		r := csv.NewReader(bytes.NewReader(raw))
		r.Comma = candidate
		record, err := r.Read()
		if err != nil {
			continue
		}
		if len(record) > fallbackWidth {
			fallback, fallbackWidth = candidate, len(record)
		}
		valid := true
		// Uma amostra de registros evita escolher pontuação presente no cabeçalho
		// que não é usada como separador nas linhas de dados.
		for i := 0; i < 20; i++ {
			_, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				valid = false
				break
			}
		}
		if valid && len(record) > 1 && len(record) > width {
			best, width = candidate, len(record)
		}
	}
	if width == 0 {
		return fallback
	}
	return best
}
