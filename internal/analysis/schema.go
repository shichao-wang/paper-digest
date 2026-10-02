package analysis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/shichao-wang/paper-digest/internal/modelchat"
)

// decode 先验证原始 JSON 树，再解码，防止 Go 零值掩盖缺失的必填字段。
func decode(data []byte, out any) error {
	if modelchat.UniqueJSON(data) != nil {
		return fmt.Errorf("%w: expected unique JSON object", ErrValidation)
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer {
		return ErrValidation
	}
	if err := validateRaw(data, t.Elem(), "$"); err != nil {
		return err
	}
	if err := modelchat.StrictDecode(data, out); err != nil {
		return fmt.Errorf("%w: incompatible JSON", ErrValidation)
	}
	return nil
}
func validateRaw(data []byte, t reflect.Type, path string) error {
	data = bytes.TrimSpace(data)
	fail := func(why string) error { return fmt.Errorf("%w: %s: %s", ErrValidation, path, why) }
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(data, []byte("null")) {
			return nil
		}
		return validateRaw(data, t.Elem(), path)
	}
	if bytes.Equal(data, []byte("null")) {
		return fail("required field cannot be null")
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil || obj == nil {
			return fail("expected object")
		}
		count := 0
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" || f.PkgPath != "" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			count++
			raw, ok := obj[name]
			if !ok {
				return fmt.Errorf("%w: %s.%s: missing required field", ErrValidation, path, name)
			}
			if err := validateRaw(raw, f.Type, path+"."+name); err != nil {
				return err
			}
		}
		if len(obj) != count {
			return fail("unexpected fields")
		}
	case reflect.Slice, reflect.Array:
		var arr []json.RawMessage
		if json.Unmarshal(data, &arr) != nil || arr == nil {
			return fail("expected array")
		}
		for i, raw := range arr {
			if err := validateRaw(raw, t.Elem(), path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case reflect.String:
		var s string
		if json.Unmarshal(data, &s) != nil {
			return fail("expected string")
		}
	case reflect.Bool:
		var v bool
		if json.Unmarshal(data, &v) != nil {
			return fail("expected boolean")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var v int64
		if json.Unmarshal(data, &v) != nil {
			return fail("expected integer")
		}
	default:
		return fail("unsupported schema type")
	}
	return nil
}

// example 给出完整键、数组元素和可空对象示例；shape 明确标注 nullable。
func example(t reflect.Type) any {
	switch t.Kind() {
	case reflect.Pointer:
		return example(t.Elem())
	case reflect.Struct:
		obj := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" || f.PkgPath != "" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			obj[name] = example(f.Type)
		}
		return obj
	case reflect.Slice:
		return []any{example(t.Elem())}
	case reflect.String:
		return "string"
	case reflect.Bool:
		return false
	case reflect.Int, reflect.Int64:
		return 1
	}
	return nil
}
func shape(t reflect.Type) any {
	if t.Kind() == reflect.Pointer {
		return map[string]any{"nullable": true, "type": shape(t.Elem())}
	}
	switch t.Kind() {
	case reflect.Struct:
		obj := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" || f.PkgPath != "" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			obj[name] = shape(f.Type)
		}
		return obj
	case reflect.Slice:
		return []any{shape(t.Elem())}
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int64:
		return "integer"
	}
	return "unsupported"
}
func schemaPrompt(value any) string {
	t := reflect.TypeOf(value)
	s, _ := json.Marshal(shape(t))
	e, _ := json.Marshal(example(t))
	return "Return exactly one JSON object with ALL fixed keys, no extra keys, no markdown. Every array is required and must be [] when empty, never null. Only fields explicitly marked nullable may be null; report unavailable information in missing_fields when that field exists. Type tree: " + string(s) + ". Complete JSON example (replace placeholder strings with grounded content, use null for unavailable nullable fields): " + string(e)
}
