package modelchat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// UniqueJSON 拒绝任意层级的重复键及尾随 JSON 值。
func UniqueJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := jsonValue(d); err != nil {
		return errors.New("invalid or duplicate-key JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("JSON must contain one value")
	}
	return nil
}
func jsonValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, compound := t.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate key")
			}
			seen[name] = true
			if err := jsonValue(d); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for d.More() {
			if err := jsonValue(d); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

type Field struct {
	Type string
	Enum []string
}

// ValidateObject 检查精确必填键、null、重复键和基本类型，不做类型转换。
// 错误只引用固定 schema 字段路径，不回传输入值或未知字段名。
func ValidateObject(data []byte, schema map[string]Field) error {
	if UniqueJSON(data) != nil {
		return errors.New("invalid JSON object")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("expected JSON object")
	}
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		field := schema[name]
		value, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("$.%s: missing or null required field", name)
		}
		switch field.Type {
		case "string":
			var s string
			if json.Unmarshal(value, &s) != nil {
				return fmt.Errorf("$.%s: expected string field", name)
			}
			if len(field.Enum) != 0 {
				ok := false
				for _, allowed := range field.Enum {
					if s == allowed {
						ok = true
					}
				}
				if !ok {
					return fmt.Errorf("$.%s: field value does not match expected evidence", name)
				}
			}
		case "boolean":
			var v bool
			if json.Unmarshal(value, &v) != nil {
				return fmt.Errorf("$.%s: expected boolean field", name)
			}
		case "integer":
			var v int64
			if json.Unmarshal(value, &v) != nil {
				return fmt.Errorf("$.%s: expected integer field", name)
			}
		default:
			return errors.New("unsupported local schema type")
		}
	}
	if len(fields) != len(schema) {
		return errors.New("$: unexpected extra field")
	}
	return nil
}

// StrictDecode 拒绝重复和未知字段；必填、null 与证据值还需 ValidateObject 检查。
func StrictDecode(data []byte, value any) error {
	if UniqueJSON(data) != nil {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return errors.New("JSON does not match local type")
	}
	return nil
}
