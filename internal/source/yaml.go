// source implements YAML loading and normalizes values for the SQL catalog.
// Author: go-smartdb maintainers. Created: 2026-10-23.
package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

func parseYAML(raw []byte) ([]map[string]any, Format, string, []string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var documents []any
	var nodes []*yaml.Node
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err == io.EOF {
			break
		} else if err != nil {
			return nil, 0, "", nil, err
		}
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, 0, "", nil, err
		}
		value, err := normalizeYAML(value)
		if err != nil {
			return nil, 0, "", nil, err
		}
		documents = append(documents, value)
		if len(node.Content) > 0 {
			nodes = append(nodes, node.Content[0])
		}
	}
	format, wrap := FormatYAMLArray, ""
	var rows []map[string]any
	var rowNodes []*yaml.Node
	if len(documents) > 1 {
		format = FormatYAMLDocuments
		for i, value := range documents {
			// Cada documento segue o mesmo mapeamento de um arquivo YAML individual.
			rs, _, _, ns := yamlRows(value, nodes[i])
			rows = append(rows, rs...)
			rowNodes = append(rowNodes, ns...)
		}
	} else if len(documents) == 1 && documents[0] != nil {
		rows, format, wrap, rowNodes = yamlRows(documents[0], nodes[0])
	}
	var order []string
	seen := map[string]bool{}
	for _, node := range rowNodes {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" {
				continue
			}
			if !seen[key.Value] {
				order = append(order, key.Value)
				seen[key.Value] = true
			}
		}
	}
	return rows, format, wrap, order, nil
}

func yamlRows(value any, node *yaml.Node) ([]map[string]any, Format, string, []*yaml.Node) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch v := value.(type) {
	case []any:
		rows, _ := toRows(v)
		return rows, FormatYAMLArray, "", node.Content
	case map[string]any:
		if len(v) == 1 {
			for key, inner := range v {
				if arr, ok := inner.([]any); ok {
					rows, _ := toRows(arr)
					for i := 0; i < len(node.Content); i += 2 {
						if node.Content[i].Value == key {
							child := node.Content[i+1]
							if child.Kind == yaml.AliasNode {
								child = child.Alias
							}
							return rows, FormatYAMLWrapped, key, child.Content
						}
					}
				}
			}
		}
	}
	return []map[string]any{asObject(value)}, FormatYAMLObject, "", []*yaml.Node{node}
}

func normalizeYAML(v any) (any, error) {
	switch x := v.(type) {
	case map[any]any:
		return nil, fmt.Errorf("mapas YAML devem usar chaves de texto")
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			normalized, err := normalizeYAML(value)
			if err != nil {
				return nil, fmt.Errorf("chave %q: %w", key, err)
			}
			out[key] = normalized
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			normalized, err := normalizeYAML(value)
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	case int:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(x, 10)), nil
	case uint64:
		return json.Number(strconv.FormatUint(x, 10)), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("YAML contém número não finito")
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		// Preserva o tipo float mesmo quando seu valor é integral.
		if !bytes.ContainsAny([]byte(s), ".eE") {
			s += ".0"
		}
		return json.Number(s), nil
	case time.Time:
		return x.Format(time.RFC3339Nano), nil
	default:
		return v, nil
	}
}
