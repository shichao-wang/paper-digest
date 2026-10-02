package modelchat

import (
	"strings"
	"testing"
)

func TestStrictEvidenceSchema(t *testing.T) {
	schema := map[string]Field{"version": {Type: "string", Enum: []string{"v2"}}, "code": {Type: "string", Enum: []string{"random-a19"}}, "missing": {Type: "boolean"}}
	valid := `{"version":"v2","code":"random-a19","missing":false}`
	if err := ValidateObject([]byte(valid), schema); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":       `{"version":"v2","missing":false}`,
		"null":          `{"version":"v2","code":null,"missing":false}`,
		"wrong_type":    `{"version":"v2","code":"random-a19","missing":"false"}`,
		"extra":         `{"version":"v2","code":"random-a19","missing":false,"secret-key":"secret-value"}`,
		"wrong_version": `{"version":"v999","code":"random-a19","missing":false}`,
		"invented_code": `{"version":"v2","code":"invented-secret-value","missing":false}`,
		"duplicate":     `{"version":"v2","code":"wrong","code":"random-a19","missing":false}`,
		"trailing":      valid + ` {}`,
		"array":         `[]`,
		"null_object":   `null`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateObject([]byte(input), schema)
			if err == nil {
				t.Fatal("invalid JSON succeeded")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "v999") {
				t.Fatalf("input leaked in error: %v", err)
			}
		})
	}
	err := ValidateObject([]byte(`{"version":"v2","missing":false}`), schema)
	if !strings.Contains(err.Error(), "$.code") {
		t.Fatalf("missing safe field path: %v", err)
	}
	if UniqueJSON([]byte(`{"nested":[{"a":1,"a":2}]}`)) == nil {
		t.Fatal("nested duplicate accepted")
	}
	var v struct {
		Version string `json:"version"`
	}
	if StrictDecode([]byte(`{"version":"v2","extra":1}`), &v) == nil {
		t.Fatal("unknown fields decoded")
	}
}
