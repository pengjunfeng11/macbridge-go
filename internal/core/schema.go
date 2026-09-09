package core

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"unicode/utf8"
)

//go:embed tools.json
var catalog []byte

func Builtins() []Tool {
	var tools []Tool
	if err := json.Unmarshal(catalog, &tools); err != nil {
		panic(err)
	}
	return tools
}
func (t Tool) MarshalJSON() ([]byte, error) {
	type plain Tool
	b, e := json.Marshal(plain(t))
	if e != nil {
		return nil, e
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	for k, v := range t.Extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}
func (t *Tool) UnmarshalJSON(b []byte) error {
	type plain Tool
	var p plain
	if e := json.Unmarshal(b, &p); e != nil {
		return e
	}
	*t = Tool(p)
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"name", "title", "description", "inputSchema", "annotations"} {
		delete(m, k)
	}
	t.Extra = m
	return nil
}

// Validate the declared input contract before entering any tool implementation.
func Validate(schema map[string]any, value any) error { return validate(schema, value, "arguments") }
func validate(s map[string]any, v any, p string) error {
	match := func(t string) bool {
		switch t {
		case "null":
			return v == nil
		case "string":
			_, ok := v.(string)
			return ok
		case "boolean":
			_, ok := v.(bool)
			return ok
		case "object":
			_, ok := v.(map[string]any)
			return ok
		case "array":
			_, ok := v.([]any)
			return ok
		case "integer":
			n, ok := v.(float64)
			return ok && math.Trunc(n) == n
		case "number":
			_, ok := v.(float64)
			return ok
		}
		return true
	}
	if t, ok := s["type"].(string); ok && !match(t) {
		return fmt.Errorf("%s must be %s", p, t)
	}
	if ts, ok := s["type"].([]any); ok {
		valid := false
		for _, t := range ts {
			if str, ok := t.(string); ok && match(str) {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("%s has invalid type", p)
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		valid := false
		for _, x := range enum {
			valid = valid || reflect.DeepEqual(v, x)
		}
		if !valid {
			return fmt.Errorf("%s has an unsupported value", p)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		if max, ok := s["maxProperties"].(float64); ok && len(x) > int(max) {
			return fmt.Errorf("%s has too many properties", p)
		}
		if req, ok := s["required"].([]any); ok {
			for _, k := range req {
				name, valid := k.(string)
				if !valid {
					return fmt.Errorf("%s schema has invalid required property", p)
				}
				if _, ok := x[name]; !ok {
					return fmt.Errorf("%s.%s is required", p, k)
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, val := range x {
			if prop, ok := props[k].(map[string]any); ok {
				if e := validate(prop, val, p+"."+k); e != nil {
					return e
				}
			} else if s["additionalProperties"] == false {
				return fmt.Errorf("unknown argument %s.%s", p, k)
			} else if sub, ok := s["additionalProperties"].(map[string]any); ok {
				if e := validate(sub, val, p+"."+k); e != nil {
					return e
				}
			}
		}
	case []any:
		if max, ok := s["maxItems"].(float64); ok && len(x) > int(max) {
			return fmt.Errorf("%s has too many items", p)
		}
		if min, ok := s["minItems"].(float64); ok && len(x) < int(min) {
			return fmt.Errorf("%s has too few items", p)
		}
		if sub, ok := s["items"].(map[string]any); ok {
			for i, v := range x {
				if e := validate(sub, v, fmt.Sprintf("%s[%d]", p, i)); e != nil {
					return e
				}
			}
		}
	case string:
		if max, ok := s["maxLength"].(float64); ok && utf8.RuneCountInString(x) > int(max) {
			return fmt.Errorf("%s is too long", p)
		}
		if min, ok := s["minLength"].(float64); ok && utf8.RuneCountInString(x) < int(min) {
			return fmt.Errorf("%s is too short", p)
		}
		if pattern, ok := s["pattern"].(string); ok {
			re, e := regexp.Compile(pattern)
			if e != nil {
				return fmt.Errorf("%s schema has invalid pattern", p)
			}
			if !re.MatchString(x) {
				return fmt.Errorf("%s has invalid format", p)
			}
		}
	case float64:
		if min, ok := s["minimum"].(float64); ok && x < min {
			return fmt.Errorf("%s below minimum", p)
		}
		if max, ok := s["maximum"].(float64); ok && x > max {
			return fmt.Errorf("%s above maximum", p)
		}
	}
	return nil
}
