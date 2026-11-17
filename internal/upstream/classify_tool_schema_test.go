package upstream

import (
	"net/http"
	"testing"
)

// TestClassifyToolSchemaRejected 11129 必须归请求级错误（ErrModelParamInvalid），
// 不能落 ErrClient——后者在 server 侧会喂连败计数（issue #114），一个坏 schema
// 能顺着轮转把整池账号逐个降权到出池（上游 misakano7545/workbuddy2api-panel#250）。
func TestClassifyToolSchemaRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ErrKind
	}{
		{
			name: "结构化 code 11129",
			body: `{"code":11129,"msg":"invalid function call parameters","requestId":"x"}`,
			want: ErrModelParamInvalid,
		},
		{
			name: "code 带空格",
			body: `{"code": 11129, "msg":"invalid function call parameters"}`,
			want: ErrModelParamInvalid,
		},
		{
			name: "displayMsg 中文文案",
			body: `{"code":11129,"displayMsg":{"zh":"工具定义不合规，请新建任务重试"}}`,
			want: ErrModelParamInvalid,
		},
		{
			name: "snake_case 错误标识",
			body: `{"error":"invalid_function_call_parameters"}`,
			want: ErrModelParamInvalid,
		},
		{
			name: "只有英文文案",
			body: `{"msg":"invalid function call parameters"}`,
			want: ErrModelParamInvalid,
		},
		{
			name: "普通 400 仍落 ErrClient",
			body: `{"code":12345,"msg":"something else"}`,
			want: ErrClient,
		},
		{
			name: "11133 不受影响",
			body: `{"code":11133,"msg":"invalid request parameters"}`,
			want: ErrModelParamInvalid,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(http.StatusBadRequest, c.body); got != c.want {
				t.Fatalf("Classify(400)=%v want %v", got, c.want)
			}
		})
	}
}

// TestClassifyToolSchemaNotOnOtherStatus 11129 只在 400 上认：其他状态码可能夹带
// 同数字的 requestId 之类，不能凭子串把别的错误改类。
func TestClassifyToolSchemaNotOnOtherStatus(t *testing.T) {
	body := `{"code":11129,"msg":"invalid function call parameters"}`
	if got := Classify(http.StatusInternalServerError, body); got == ErrModelParamInvalid {
		t.Fatalf("500 不应判成 ErrModelParamInvalid")
	}
}
