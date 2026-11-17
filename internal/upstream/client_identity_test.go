// client_identity_test.go X-Client-* 三头的注入 / 剥离 / realm 分档单测。
package upstream

import (
	"net/http"
	"runtime"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// cnAuth / globalAuth 按 domain 推断 realm（与生产一致），不直接写 realm 字段。
func cnIdentityAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.cn"}
}
func globalIdentityAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.workbuddy.ai"}
}

func TestClientPlatformFor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"darwin", "macos"},
		{"MacOS", "macos"},
		{"windows", "windows"},
		{"win32", "windows"},
		{"linux", "linux"},
		{"freebsd", ""},
		{"", ""},
	} {
		if got := clientPlatformFor(tc.in); got != tc.want {
			t.Errorf("clientPlatformFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// newIdentityClient 打开身份的 Client（生产 NewClient 的默认态）。
func newIdentityClient() *Client {
	c := &Client{ChatBaseCN: "https://chat.example", BillingBaseCN: "https://billing.example"}
	c.ClientIdentityHeaders.Store(true)
	return c
}

func TestClientIdentity_CNCommonHeaders(t *testing.T) {
	c := newIdentityClient()
	req, _ := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v3/config", nil)
	a := cnIdentityAuth()
	c.CommonHeaders(req, a)

	if got := req.Header.Get(headerClientVersion); got != defaultClientVersionCN {
		t.Errorf("X-Client-Version = %q, want %q", got, defaultClientVersionCN)
	}
	if got := req.Header.Get(headerClientProduct); got != clientProductCN {
		t.Errorf("X-Client-Product = %q, want %q", got, clientProductCN)
	}
	if got, want := req.Header.Get(headerClientPlatform), clientPlatformFor(runtime.GOOS); got != want {
		t.Errorf("X-Client-Platform = %q, want %q", got, want)
	}
}

// ClientVersion 显式覆盖时头与 UA 自洽。
func TestClientIdentity_UsesConfiguredVersion(t *testing.T) {
	c := newIdentityClient()
	c.ClientVersion = "5.12.0"
	req, _ := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v3/config", nil)
	c.CommonHeaders(req, cnIdentityAuth())
	if got := req.Header.Get(headerClientVersion); got != "5.12.0" {
		t.Errorf("X-Client-Version = %q, want 5.12.0", got)
	}
}

// 国际版 5.9.4 真机实测不发这组头。
func TestClientIdentity_GlobalSkips(t *testing.T) {
	c := newIdentityClient()
	req, _ := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v3/config", nil)
	c.CommonHeaders(req, globalIdentityAuth())
	for _, h := range []string{headerClientVersion, headerClientPlatform, headerClientProduct} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("global 不应带 %s，得到 %q", h, got)
		}
	}
}

// 开关关闭 = 完全还原（CommonHeaders / BillingHeaders 都不注入）。
func TestClientIdentity_Disabled(t *testing.T) {
	c := newIdentityClient()
	c.ClientIdentityHeaders.Store(false)
	req, _ := http.NewRequest(http.MethodPost, c.BillingBaseCN+"/v2/report", nil)
	c.BillingHeaders(req, cnIdentityAuth())
	for _, h := range []string{headerClientVersion, headerClientPlatform, headerClientProduct} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("开关关闭后不应带 %s，得到 %q", h, got)
		}
	}
}

func TestClientIdentity_BillingHeaders(t *testing.T) {
	c := newIdentityClient()
	req, _ := http.NewRequest(http.MethodPost, c.BillingBaseCN+"/v2/report", nil)
	c.BillingHeaders(req, cnIdentityAuth())
	if got := req.Header.Get(headerClientVersion); got != defaultClientVersionCN {
		t.Errorf("billing X-Client-Version = %q, want %q", got, defaultClientVersionCN)
	}
}

// chat 链必须剥离：内置 CLI 把这三个头列在待剥离名单里，零发送点。
func TestClientIdentity_ChatStrips(t *testing.T) {
	c := newIdentityClient()
	req, _ := http.NewRequest(http.MethodPost, c.ChatBaseCN+"/v2/chat/completions", nil)
	c.ChatHeaders(req, cnIdentityAuth(), "", ChatMeta{})
	for _, h := range []string{headerClientVersion, headerClientPlatform, headerClientProduct} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("chat 不应带 %s，得到 %q", h, got)
		}
	}
}

// 零值 Client（未 Store）不注入，避免测试/内部裸构造悄悄改变出站形态。
func TestClientIdentity_ZeroValueClientInjectsNothing(t *testing.T) {
	c := &Client{ChatBaseCN: "https://chat.example"}
	req, _ := http.NewRequest(http.MethodGet, c.ChatBaseCN+"/v3/config", nil)
	c.CommonHeaders(req, cnIdentityAuth())
	if got := req.Header.Get(headerClientVersion); got != "" {
		t.Errorf("零值 Client 不应注入，得到 %q", got)
	}
}
