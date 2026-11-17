package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 技能分享（成长任务 skill_share）走的是两个不在配置里、也不随 realm 变化的域：
//
//   - 技能市场          www.codebuddy.cn  /v2/operation-platform/market/skill/list
//   - 开放平台资产分享  open.workbuddy.cn /api/v2/open-platform/asset-shares/c2c/channels
//
// 两处都是 2026-11 从同类实现（niyouqianma/workbuddy2api-hub 8eede085）与官方
// 桌面端行为对齐的口径，本仓尚未真机复核；因此调用点把它当作「尝试型」任务，
// 失败只影响这一个动作，不动号池惩罚计数。
const (
	codeBuddyBaseDefault = "https://www.codebuddy.cn"
	openBaseDefault      = "https://open.workbuddy.cn"

	skillMarketPath = "/v2/operation-platform/market/skill/list"
	assetSharePath  = "/api/v2/open-platform/asset-shares/c2c/channels"
	// shareChannelHost 分享渠道接口的 X-Domain 口径（不是 URL 主机名）。
	shareChannelHost = "copilot.tencent.com"
	marketHost       = "www.codebuddy.cn"
	// PreferredShareSkillID 是实测能被计分的那条市场技能；市场里没有它时退回列表第一条。
	PreferredShareSkillID = "skill_2097350077599879168"
)

// MarketSkill 技能市场的一条技能。上游同一字段有两种命名（snake/camel、zh/en），
// 这里都收，取值时按优先级回落。
type MarketSkill struct {
	SkillID  string `json:"skill_id"`
	ID       string `json:"id"`
	NameZH   string `json:"display_name_zh"`
	NameEN   string `json:"display_name_en"`
	Name     string `json:"name"`
	DescZH   string `json:"description_zh"`
	DescEN   string `json:"description_en"`
	Desc     string `json:"description"`
	SkillVer string `json:"version"`
}

// Key 技能标识（分享渠道的 asset_id / shareKey / 事件 skillId 都用它）。
func (s MarketSkill) Key() string {
	return firstNonEmptyStr(s.SkillID, s.ID)
}

// Title 展示名（写进渠道 extra.name 与事件 skillName）。
func (s MarketSkill) Title() string {
	return firstNonEmptyStr(s.NameZH, s.Name, s.NameEN, s.Key())
}

// Summary 简介，截断到 60 字符与官方分享卡片一致。
func (s MarketSkill) Summary() string {
	d := firstNonEmptyStr(s.DescZH, s.Desc, s.DescEN)
	if r := []rune(d); len(r) > 60 {
		return string(r[:60])
	}
	return d
}

// ShareChannel 分享渠道创建结果。
type ShareChannel struct {
	ShareCode string
	ChannelID string
	Link      string
}

// codeBuddyBase / openBase 允许测试覆盖：面板测试在 package panel 里，改不到
// upstream 的包级变量，只能靠 Client 字段。
func (c *Client) codeBuddyBase() string {
	if c != nil && c.CodeBuddyBaseCN != "" {
		return strings.TrimRight(c.CodeBuddyBaseCN, "/")
	}
	return codeBuddyBaseDefault
}

func (c *Client) openBase() string {
	if c != nil && c.OpenBaseCN != "" {
		return strings.TrimRight(c.OpenBaseCN, "/")
	}
	return openBaseDefault
}

// MarketSkillList 读技能市场第一页（20 条）。
func (c *Client) MarketSkillList(a *auth.Auth) ([]MarketSkill, error) {
	body, _ := json.Marshal(map[string]any{"page": 1, "page_size": 20})
	req, err := http.NewRequest(http.MethodPost, c.codeBuddyBase()+skillMarketPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Domain", marketHost)

	var out struct {
		Skills []MarketSkill `json:"skills"`
	}
	if _, err := c.postJSONAbsolute(req, a, &out); err != nil {
		return nil, err
	}
	return out.Skills, nil
}

// CreateSkillShareChannel 在开放平台创建一条真实的技能分享渠道。
func (c *Client) CreateSkillShareChannel(a *auth.Auth, skill MarketSkill) (ShareChannel, error) {
	key := skill.Key()
	if key == "" {
		return ShareChannel{}, fmt.Errorf("skill id is empty")
	}
	assetRef, _ := json.Marshal(map[string]string{"asset_id": key})
	body, err := json.Marshal(map[string]any{
		"asset_kind":   "skill",
		"asset_source": "market",
		"asset_ref":    string(assetRef),
		"ways":         []string{"link", "wxacode", "wxapath"},
		"extra": map[string]any{
			"name":              skill.Title(),
			"description":       skill.Summary(),
			"shareKey":          key,
			"marketplaceSource": "builtin-market",
		},
	})
	if err != nil {
		return ShareChannel{}, err
	}
	req, err := http.NewRequest(http.MethodPost, c.openBase()+assetSharePath, bytes.NewReader(body))
	if err != nil {
		return ShareChannel{}, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Domain", shareChannelHost)

	var out struct {
		ShareCode string `json:"share_code"`
		ChannelID string `json:"channel_id"`
		Ways      []struct {
			ShareWay    string `json:"share_way"`
			ResourceRef string `json:"resource_ref"`
		} `json:"ways"`
	}
	if _, err := c.postJSONAbsolute(req, a, &out); err != nil {
		return ShareChannel{}, err
	}
	if out.ShareCode == "" {
		return ShareChannel{}, fmt.Errorf("share channel response has no share_code")
	}
	res := ShareChannel{ShareCode: out.ShareCode, ChannelID: out.ChannelID}
	for _, w := range out.Ways {
		if w.ShareWay == "link" {
			res.Link = w.ResourceRef
			break
		}
	}
	return res, nil
}

// postJSONAbsolute 发一个已构造好的绝对地址请求，并把信封 data 解进 out。
//
// 为什么不直接用 doJSON：这两个接口的 code 既可能是数字 0，也可能是数字 200 或
// 字符串（同类实现里两种都见过）。doJSON 的信封把 code 定成 int 且只认 0，遇到
// 变体会把成功判成失败；这里按「0 / 200 / 空」宽松认，其余才报错。
func (c *Client) postJSONAbsolute(req *http.Request, a *auth.Auth, out any) (json.RawMessage, error) {
	resp, err := c.do(req, a, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if raw, err = decodeResponseBody(resp, raw); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if !envelopeCodeOK(env.Code) {
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%s msg=%s", strings.TrimSpace(string(env.Code)), truncate(env.Msg, 160))}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return nil, fmt.Errorf("decode data: %w (body: %s)", err, truncate(string(env.Data), 120))
		}
	}
	return env.Data, nil
}

// envelopeCodeOK 认 0 / 200 / 缺省（平铺响应）三种成功形态。
func envelopeCodeOK(raw json.RawMessage) bool {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return s == "" || s == "0" || s == "200"
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
