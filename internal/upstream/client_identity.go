// client_identity.go 复刻桌面端 5.11.3 主进程的 X-Client-* 出站头族。
//
// 来源：5.11.3 的 app.asar 新增 `proxy-env2` 的 registerOutboundHeaderProvider /
// getOutboundHeaders 机制，由 `main/client-identity.js` 注册一个 id 为
// `client-identity` 的来源，origin 限定为 product config 的 endpoint
// （CN = https://www.workbuddy.cn）。注入点挂在三处：
//   - undici 全局 dispatcher（installUndiciProxyDispatcher 的 PAC 与直连两个分支
//     都用 withOutboundHeaders 包了，不是 PAC 专属）；
//   - 全局 axios 请求拦截器；
//   - WorkbuddyRestOperationsProxyInterceptor（RPC 族的 restOperations）。
//
// 真机实测（2026-11-17，同一台机器上两个客户端各跑一次）：
//
//	国内版 5.11.3  GET /v3/config、POST /v2/report（NetLog 均为 source=RestOperations）
//	              头列表末尾追加：
//	                X-Client-Version: 5.11.3
//	                X-Client-Platform: macos
//	                X-Client-Product: WorkBuddy
//	国际版 5.9.4  对 https://www.workbuddy.ai 与 https://www.workbuddy.cn 均不发
//	              （该版本 app.asar 里没有这层机制，x-client 一条都没有）
//
// 取值口径（复刻 buildClientIdentityHeaders）：
//   - Version  = electron.app.getVersion()，即 desktop app 版本，不是 CLI 版本；
//   - Platform = process.platform 归一化（darwin/mac→macos、win32→windows、
//     linux→linux），所以这里用 runtime.GOOS 走同一套规则，不写死；
//   - Product  = product config 的 applicationName，CN 为 WorkBuddy；
//     代码里的兜底值是 workbuddy-desktop，仅当取不到 product config 时才出现。
//
// chat 链**不注入**：内置 CLI 子进程把这三个头列在 isSkippableInternalHeader 的
// 待剥离名单里（`eq` 数组含 CLIENT_VERSION_HEADER / CLIENT_PLATFORM_HEADER /
// CLIENT_PRODUCT_HEADER），全量扫 cli/ 目录零发送点。ChatHeaders 因此显式 Del，
// 防止 CommonHeaders 的注入漏到 chat 上——「CLI 链带主进程头」比缺头更像异常客户端。
package upstream

import (
	"net/http"
	"runtime"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const (
	headerClientVersion  = "X-Client-Version"
	headerClientPlatform = "X-Client-Platform"
	headerClientProduct  = "X-Client-Product"

	// clientProductCN 国内版 product.json 的 applicationName（真机实测值 WorkBuddy）。
	// 国际版 product.json 是 workbuddy-ai，但国际版不发这组头，故不参与。
	clientProductCN = "WorkBuddy"
)

// clientPlatformFor 复刻客户端 toWorkbuddyClientPlatform：按运行平台归一。
// 未识别的平台返回空串，调用方据此不发该头（与 JS 返回 undefined 的行为一致）。
func clientPlatformFor(goos string) string {
	s := strings.ToLower(goos)
	switch {
	case strings.Contains(s, "darwin"), strings.Contains(s, "mac"):
		return "macos"
	case strings.Contains(s, "win"):
		return "windows"
	case strings.Contains(s, "linux"):
		return "linux"
	}
	return ""
}

// injectClientIdentity 在 RPC 族请求上补三个客户端身份头。
//
// 只对 CN realm 生效：国际版 5.9.4 真机实测对 www.workbuddy.ai 与 www.workbuddy.cn
// 都不发，给国际号补这组头反而会造出官方客户端不存在的形态。
//
// Version 用 c.clientVersion(a) 而不是常量：它同时是该账号 UA 里的版本段，用户把
// client_version 改成别的值时头与 UA 仍然自洽。
func (c *Client) injectClientIdentity(req *http.Request, a *auth.Auth) {
	if c == nil || !c.ClientIdentityHeaders.Load() {
		return
	}
	if a != nil && a.IsGlobal() {
		return
	}
	req.Header.Set(headerClientVersion, c.clientVersion(a))
	if p := clientPlatformFor(runtime.GOOS); p != "" {
		req.Header.Set(headerClientPlatform, p)
	}
	req.Header.Set(headerClientProduct, clientProductCN)
}

// stripClientIdentity 供 chat 链调用：把 CommonHeaders 可能已注入的三个头摘掉。
// 无注入时是空操作（Del 不存在的键无害），所以不加条件判断。
func stripClientIdentity(req *http.Request) {
	req.Header.Del(headerClientVersion)
	req.Header.Del(headerClientPlatform)
	req.Header.Del(headerClientProduct)
}
