package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// skillShareFake 假上游：技能市场 + 开放平台分享渠道 + 事件上报三个端点。
type skillShareFake struct {
	mu           sync.Mutex
	skills       []map[string]any
	marketCode   any // 市场响应里的 code（默认 0，可设成 "200" 验证宽松判定）
	shareCode    string
	omitShare    bool
	marketCalls  int
	channelCalls int
	channelBody  map[string]any
	reported     []map[string]any
}

func (f *skillShareFake) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/v2/operation-platform/market/skill/list":
			f.marketCalls++
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			code := f.marketCode
			if code == nil {
				code = 0
			}
			out, _ := json.Marshal(map[string]any{
				"code": code, "msg": "OK",
				"data": map[string]any{"skills": f.skills},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case "/api/v2/open-platform/asset-shares/c2c/channels":
			f.channelCalls++
			_ = json.Unmarshal(raw, &f.channelBody)
			data := map[string]any{"channel_id": "ch-1"}
			if !f.omitShare {
				data["share_code"] = f.shareCode
				data["ways"] = []map[string]any{
					{"share_way": "link", "resource_ref": "https://example.test/s/" + f.shareCode},
				}
			}
			out, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": data})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case "/v2/report":
			var evs []map[string]any
			_ = json.Unmarshal(raw, &evs)
			f.reported = append(f.reported, evs...)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"msg":"OK","data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func newSkillSharePanel(t *testing.T, f *skillShareFake) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	client := upstream.New()
	client.ChatBaseCN, client.WebBaseCN, client.BillingBaseCN = srv.URL, srv.URL, srv.URL
	// 两个绝对地址的域必须一起指过去，否则请求会打到真实外网。
	client.CodeBuddyBaseCN, client.OpenBaseCN = srv.URL, srv.URL
	return New(Config{Upstream: client}), &auth.Auth{UID: "skill-share-test", AccessToken: "token"}
}

func skillRow(id, name string) map[string]any {
	return map[string]any{
		"skill_id": id, "display_name_zh": name, "description_zh": "简介 " + name,
	}
}

// TestSkillSharePreferredSkillAndEvents：整条链——选实测技能、建真实渠道、用
// share_code 报两条渠道动作事件。
func TestSkillSharePreferredSkillAndEvents(t *testing.T) {
	f := &skillShareFake{
		skills: []map[string]any{
			skillRow("skill-other", "别的技能"),
			skillRow(upstream.PreferredShareSkillID, "润泽小馆·日报撰写"),
		},
		shareCode: "sc-abc123",
	}
	p, a := newSkillSharePanel(t, f)

	msg, err := runSkillShare(p, a)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if msg == "" {
		t.Fatal("没有返回说明文案")
	}
	if f.marketCalls != 1 || f.channelCalls != 1 {
		t.Fatalf("calls market=%d channel=%d", f.marketCalls, f.channelCalls)
	}
	if got := f.channelBody["asset_kind"]; got != "skill" {
		t.Fatalf("asset_kind=%v", got)
	}
	if got := f.channelBody["asset_source"]; got != "market" {
		t.Fatalf("asset_source=%v", got)
	}
	// asset_ref 是一段 JSON 字符串：{ "asset_id": "<id>" }
	ref, _ := f.channelBody["asset_ref"].(string)
	var parsed map[string]string
	if err := json.Unmarshal([]byte(ref), &parsed); err != nil {
		t.Fatalf("asset_ref 不是 JSON: %q", ref)
	}
	if parsed["asset_id"] != upstream.PreferredShareSkillID {
		t.Fatalf("没优先选实测技能: %v", parsed)
	}
	if len(f.reported) != 2 {
		t.Fatalf("事件数=%d", len(f.reported))
	}
	for i, ev := range f.reported {
		if ev["eventCode"] != "skill_share_channel_action" {
			t.Fatalf("第 %d 条 eventCode=%v", i, ev["eventCode"])
		}
		if ev["id"] != "sc-abc123" {
			t.Fatalf("第 %d 条 id 不是 share_code: %v", i, ev["id"])
		}
		if ev["skillId"] != upstream.PreferredShareSkillID {
			t.Fatalf("第 %d 条 skillId=%v", i, ev["skillId"])
		}
		if ev["isSuccessful"] != true {
			t.Fatalf("第 %d 条 isSuccessful=%v", i, ev["isSuccessful"])
		}
	}
	if f.reported[0]["panelAction"] != "click" || f.reported[1]["panelAction"] != "success" {
		t.Fatalf("两条事件的 panelAction 不对: %v / %v", f.reported[0]["panelAction"], f.reported[1]["panelAction"])
	}
}

// TestSkillShareFallsBackToFirstSkill：没有实测技能时用列表第一条，不能空转。
func TestSkillShareFallsBackToFirstSkill(t *testing.T) {
	f := &skillShareFake{
		skills:    []map[string]any{skillRow("", "无 id 技能"), skillRow("skill-x", "备用技能")},
		shareCode: "sc-fallback",
	}
	p, a := newSkillSharePanel(t, f)
	if _, err := runSkillShare(p, a); err != nil {
		t.Fatalf("run: %v", err)
	}
	ref, _ := f.channelBody["asset_ref"].(string)
	var parsed map[string]string
	_ = json.Unmarshal([]byte(ref), &parsed)
	if parsed["asset_id"] != "skill-x" {
		t.Fatalf("回落选错技能: %v", parsed)
	}
}

// TestSkillShareNoMarketSkillStops：市场为空时直接失败，不建渠道、不报事件。
func TestSkillShareNoMarketSkillStops(t *testing.T) {
	f := &skillShareFake{skills: nil, shareCode: "unused"}
	p, a := newSkillSharePanel(t, f)
	if _, err := runSkillShare(p, a); err == nil {
		t.Fatal("市场为空却成功了")
	}
	if f.channelCalls != 0 || len(f.reported) != 0 {
		t.Fatalf("市场为空仍建了渠道/报了事件: channel=%d events=%d", f.channelCalls, len(f.reported))
	}
}

// TestSkillShareMissingShareCodeStops：没有 share_code 就不该伪造事件。
func TestSkillShareMissingShareCodeStops(t *testing.T) {
	f := &skillShareFake{
		skills:    []map[string]any{skillRow("skill-a", "技能 A")},
		shareCode: "",
		omitShare: true,
	}
	p, a := newSkillSharePanel(t, f)
	if _, err := runSkillShare(p, a); err == nil {
		t.Fatal("没有 share_code 却成功了")
	}
	if len(f.reported) != 0 {
		t.Fatalf("没有 share_code 仍上报了 %d 条事件", len(f.reported))
	}
}

// TestSkillShareAcceptsStringCode：接口把 code 写成字符串 "200" 也算成功。
func TestSkillShareAcceptsStringCode(t *testing.T) {
	f := &skillShareFake{
		skills:     []map[string]any{skillRow("skill-a", "技能 A")},
		marketCode: "200",
		shareCode:  "sc-str",
	}
	p, a := newSkillSharePanel(t, f)
	if _, err := runSkillShare(p, a); err != nil {
		t.Fatalf("字符串 200 被当成失败: %v", err)
	}
	if len(f.reported) != 2 {
		t.Fatalf("事件数=%d", len(f.reported))
	}
}

// TestSkillShareRegisteredInAutoActions：注册表里必须真有这条，否则面板跑不到它。
func TestSkillShareRegisteredInAutoActions(t *testing.T) {
	act := autoActionFor("skill_share")
	if act == nil || act.run == nil {
		t.Fatal("autoActions 里没有 skill_share 的可执行动作")
	}
	if act.ClaimOnly {
		t.Fatal("skill_share 有真实动作，不该标成只领奖")
	}
	if !act.Attempt {
		t.Fatal("未真机复核的动作应标 Attempt（面板会显示为尝试型）")
	}
}
