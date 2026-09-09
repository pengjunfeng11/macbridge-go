package core

import "testing"

func TestDeclaredConstraints(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"env"}, "properties": map[string]any{"env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": float64(2)}}}}
	if e := Validate(schema, map[string]any{"env": map[string]any{"A": "中文"}}); e != nil {
		t.Fatal(e)
	}
	for _, v := range []any{map[string]any{}, map[string]any{"env": map[string]any{"A": 42.0}}, map[string]any{"env": map[string]any{"A": "中文文"}}} {
		if Validate(schema, v) == nil {
			t.Fatalf("accepted invalid input: %#v", v)
		}
	}
	if Validate(map[string]any{"required": []any{3.0}}, map[string]any{}) == nil {
		t.Fatal("invalid child schema accepted")
	}
	if Validate(map[string]any{"pattern": "["}, "text") == nil {
		t.Fatal("invalid pattern accepted")
	}
	if Validate(map[string]any{"maxItems": float64(1)}, []any{1, 2}) == nil {
		t.Fatal("array limit ignored")
	}
}
