package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// normalizeToolsJSON 跑一遍 normalizeToolPatterns 并把结果重新序列化，便于与期望
// JSON 做结构化比较（数字统一落 float64，避免手写类型踩坑）。
func normalizeToolsJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	normalizeToolPatterns(body)
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal normalised body: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal normalised body: %v", err)
	}
	return back
}

func mustJSONMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal expected: %v", err)
	}
	return m
}

// TestNormalizeToolSchemaKeywordTypes 覆盖同类仓库实测会触发上游 11129 的关键字
// 写法（niyouqianma/workbuddy2api-hub PR #885 的 26 种对拍清单）。
func TestNormalizeToolSchemaKeywordTypes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "数字关键字字符串化",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":"1","maximum":"10.5","multipleOf":"0.5","minLength":"2"}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":1,"maximum":10.5,"multipleOf":0.5,"minLength":2}}}}}]}`,
		},
		{
			name: "数字关键字解析失败则删除",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"integer","minimum":"abc","maximum":null}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"integer"}}}}}]}`,
		},
		{
			name: "布尔关键字字符串化",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"array","uniqueItems":"true","readOnly":"1","writeOnly":0,"deprecated":"maybe"}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"array","uniqueItems":true,"readOnly":true,"writeOnly":false}}}}}]}`,
		},
		{
			name: "字符串关键字 null 删除、数字转字符串",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","description":null,"title":123,"format":"date","pattern":"^a$"}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","title":"123","format":"date","pattern":"^a$"}}}}}]}`,
		},
		{
			name: "enum 非数组删除、数组保留",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","enum":"x,y"},"b":{"enum":["x","y"]}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"enum":["x","y"],"type":"string"}}}}}]}`,
		},
		{
			name: "additionalProperties 字符串转布尔",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","additionalProperties":"true","properties":{}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","additionalProperties":true,"properties":{}}}}]}`,
		},
		{
			name: "additionalProperties 无法解析则删除",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","additionalProperties":"maybe","properties":{}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{}}}}]}`,
		},
		{
			name: "additionalProperties 对象递归纠偏",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","additionalProperties":{"type":"object","properties":{"a":{"type":"int","minimum":"0"}}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","additionalProperties":{"type":"object","properties":{"a":{"type":"integer","minimum":0}}}}}}]}`,
		},
		{
			name: "Python 风格类型别名",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"str"},"b":{"type":"dict"},"c":{"type":"list"},"d":{"type":"bool"},"e":{"type":"float"},"f":{"type":"None"}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"object"},"c":{"type":"array"},"d":{"type":"boolean"},"e":{"type":"number"},"f":{"type":"null"}}}}}]}`,
		},
		{
			name: "工具自身 description 纠偏",
			in:   `{"tools":[{"function":{"name":"f","description":null,"parameters":{"type":"object","properties":{}}}},{"function":{"name":"g","description":5,"parameters":{"type":"object","properties":{}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{}}}},{"function":{"name":"g","description":"5","parameters":{"type":"object","properties":{}}}}]}`,
		},
		{
			name: "嵌套 items 与数组关键字",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"array","minItems":"2","maxItems":"abc","items":{"type":"object","properties":{"b":{"type":"str"}}}}}}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"array","minItems":2,"items":{"type":"object","properties":{"b":{"type":"string"}}}}}}}}]}`,
		},
		{
			name: "合规 schema 不动",
			in:   `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","description":"ok","enum":["x"]}},"required":["a"]}}}]}`,
			want: `{"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string","description":"ok","enum":["x"]}},"required":["a"]}}}]}`,
		},
		{
			name: "tools 之外的消息内容不受影响",
			in:   `{"messages":[{"role":"user","content":"type=str minimum=1"}],"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"str"}}}}}]}`,
			want: `{"messages":[{"role":"user","content":"type=str minimum=1"}],"tools":[{"function":{"name":"f","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeToolsJSON(t, c.in)
			want := mustJSONMap(t, c.want)
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.Marshal(got)
				t.Fatalf("normalized mismatch\n got: %s\nwant: %s", gotJSON, c.want)
			}
		})
	}
}

// TestNormalizeToolSchemaIdempotent 二次执行必须与一次相同：请求可能经过多段
// （首段 + 换段 + 重试）重复准备。
func TestNormalizeToolSchemaIdempotent(t *testing.T) {
	raw := `{"tools":[{"function":{"name":"f","description":null,"parameters":{"type":"object","additionalProperties":"true","properties":{"a":{"type":"str","minimum":"1","enum":"x","uniqueItems":"yes"}}}}}]}`
	once := normalizeToolsJSON(t, raw)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	normalizeToolPatterns(body)
	normalizeToolPatterns(body)
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var twice map[string]any
	if err := json.Unmarshal(out, &twice); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(once, twice) {
		a, _ := json.Marshal(once)
		b, _ := json.Marshal(twice)
		t.Fatalf("not idempotent\n once: %s\n twice: %s", a, b)
	}
}
