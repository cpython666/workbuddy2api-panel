package upstream

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// 工具 Schema 关键字纠偏（上游 code 11129「工具定义不合规」）。
//
// 上游对 tools[].function.parameters 做 Go 强类型反序列化，关键字值类型不符就整条
// 请求 400 + code 11129，而不是退回宽松解析。实测会触发的写法（同类仓库
// niyouqianma/workbuddy2api-hub PR #885 逐条对拍 26 种）：
//
//	"minimum": "1"           数字关键字给了字符串
//	"uniqueItems": "true"    布尔关键字给了字符串
//	"description": null      字符串关键字给了 null
//	"type": "str"            Python 风格类型别名
//	"enum": "a,b"            数组关键字给了字符串
//
// 这里的修复原则与 normalizeToolPatterns 一致：**只做无损规整**。能从字面值看出
// 类型的就转（"1" → 1、"true" → true），看不出来就删掉——留着一定 400，删掉只是少
// 一条约束，不影响工具语义；绝不臆造新的值。
//
// 只在 tools 子树调用（normalizeSchemaTree 的入口），消息正文与其他字段不碰。

// schemaNumberKeywords JSON Schema 里必须是数字的关键字。
var schemaNumberKeywords = []string{
	"minimum", "maximum",
	"exclusiveMinimum", "exclusiveMaximum",
	"multipleOf",
	"minLength", "maxLength",
	"minItems", "maxItems",
	"minProperties", "maxProperties",
	"minContains", "maxContains",
}

// schemaBoolKeywords 必须是布尔的关键字。
var schemaBoolKeywords = []string{
	"uniqueItems", "deprecated", "readOnly", "writeOnly",
}

// schemaStringKeywords 必须是字符串的关键字（null 删除，其余转字符串）。
var schemaStringKeywords = []string{
	"description", "title", "format", "pattern", "$comment",
}

// hardenSchemaKeywords 就地把单个 schema 节点的关键字值类型纠偏。
// 幂等：对已合规的节点是空操作。
func hardenSchemaKeywords(n map[string]any) {
	for _, k := range schemaNumberKeywords {
		v, ok := n[k]
		if !ok {
			continue
		}
		if f, ok := schemaNumber(v); ok {
			n[k] = f
			continue
		}
		delete(n, k)
	}
	for _, k := range schemaBoolKeywords {
		v, ok := n[k]
		if !ok {
			continue
		}
		if b, ok := schemaBool(v); ok {
			n[k] = b
			continue
		}
		delete(n, k)
	}
	for _, k := range schemaStringKeywords {
		v, ok := n[k]
		if !ok {
			continue
		}
		if s, ok := schemaText(v); ok {
			n[k] = s
			continue
		}
		delete(n, k)
	}
	// enum / examples：非数组整条删。数组里的元素不动（值本身可以是任意 JSON）。
	for _, k := range []string{"enum", "examples"} {
		if v, ok := n[k]; ok {
			if _, isArr := v.([]any); !isArr {
				delete(n, k)
			}
		}
	}
	// additionalProperties：布尔或 schema 对象；字符串数字先转布尔，其他删。
	if v, ok := n["additionalProperties"]; ok {
		switch v.(type) {
		case bool, map[string]any:
		case string, float64, json.Number:
			if b, ok := schemaBool(v); ok {
				n["additionalProperties"] = b
			} else {
				delete(n, "additionalProperties")
			}
		default:
			delete(n, "additionalProperties")
		}
	}
	// type：Python 风格别名映射到 JSON Schema 类型名。
	if t, ok := n["type"].(string); ok {
		if alias := schemaTypeAlias(t); alias != "" {
			n["type"] = alias
		}
	}
}

// schemaNumber 把数字表现形式转成 float64；字符串能解析就用，解析不出返回 false。
// NaN / Inf 一律拒绝：JSON 无法表示，且上游结构体也不接受。
func schemaNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return finite(x)
	case float32:
		return finite(float64(x))
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		return finite(f)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return finite(f)
	}
	return 0, false
}

func finite(f float64) (float64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// schemaBool 布尔纠偏：bool 原样；字符串认 true/false/1/0/yes/no；数字只认 0/1。
func schemaBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	case float64:
		if x == 1 {
			return true, true
		}
		if x == 0 {
			return false, true
		}
	case json.Number:
		if x == "1" {
			return true, true
		}
		if x == "0" {
			return false, true
		}
	}
	return false, false
}

// schemaText 字符串纠偏：字符串原样；数字/布尔按字面量转；null 与复合值返回 false
// （调用方删除该关键字）。json.Number 保留原始字面量，避免 1.0 被写成 1。
func schemaText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		if f, ok := finite(x); ok {
			return strconv.FormatFloat(f, 'f', -1, 64), true
		}
	case json.Number:
		return x.String(), true
	}
	return "", false
}

// schemaTypeAlias Python/弱类型语言的类型别名 → JSON Schema 类型名。非别名的原样
// 返回空串（不改写）。只认小写精确匹配，避免把用户自定义的枚举名改坏。
func schemaTypeAlias(t string) string {
	switch t {
	case "str":
		return "string"
	case "int":
		return "integer"
	case "float", "double":
		return "number"
	case "bool":
		return "boolean"
	case "list", "tuple":
		return "array"
	case "dict", "map":
		return "object"
	case "None":
		return "null"
	}
	return ""
}

// coerceFunctionDescription 工具自身的 description 非字符串时按字面量转字符串，
// null 删除：上游对 tools[].function.description 同样要求字符串类型。
func coerceFunctionDescription(fn map[string]any) {
	if fn == nil {
		return
	}
	v, ok := fn["description"]
	if !ok {
		return
	}
	if _, isStr := v.(string); isStr {
		return
	}
	if s, ok := schemaText(v); ok {
		fn["description"] = s
		return
	}
	delete(fn, "description")
}
