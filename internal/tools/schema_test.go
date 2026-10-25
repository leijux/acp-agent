package tools

import (
	"encoding/json"
	"testing"
)

// TestAllSchemasAreValidObjects 守住一个真实踩过的坑：零参数工具
// Parameters() 返回 Schema(nil) 时，properties 会被序列化成 null，
// OpenAI 直接回 400 "Invalid schema for function 'context_check':
// null is not of type \"object\""。
func TestAllSchemasAreValidObjects(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(ContextTools(nil)...); err != nil {
		t.Fatalf("register: %v", err)
	}

	specs := reg.Specs()
	if len(specs) == 0 {
		t.Fatal("no tools registered")
	}
	for _, spec := range specs {
		tool, ok := reg.Get(spec.Name)
		if !ok {
			t.Fatalf("%s: missing from registry", spec.Name)
		}
		params := tool.Parameters()
		if params == nil {
			t.Errorf("%s: Parameters() = nil", tool.Name())
			continue
		}
		if got := params["type"]; got != "object" {
			t.Errorf("%s: type = %v, want object", tool.Name(), got)
		}
		props, ok := params["properties"]
		if !ok {
			t.Errorf("%s: missing properties", tool.Name())
			continue
		}
		if props == nil {
			t.Errorf("%s: properties is nil (extra nil, not omitted)", tool.Name())
			continue
		}
		if _, ok := props.(map[string]any); !ok {
			t.Errorf("%s: properties type = %T, want map[string]any", tool.Name(), props)
		}

		// 真正的判据：序列化结果里不能出现 null。
		raw, err := json.Marshal(params)
		if err != nil {
			t.Errorf("%s: marshal: %v", tool.Name(), err)
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Errorf("%s: unmarshal: %v", tool.Name(), err)
			continue
		}
		if _, ok := decoded["properties"].(map[string]any); !ok {
			t.Errorf("%s: JSON properties = %s, want object", tool.Name(), raw)
		}
	}
}

func TestSchemaNilPropertiesIsEmptyObject(t *testing.T) {
	raw, err := json.Marshal(Schema(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"additionalProperties":false,"properties":{},"type":"object"}`
	if string(raw) != want {
		t.Fatalf("Schema(nil) = %s, want %s", raw, want)
	}
}
