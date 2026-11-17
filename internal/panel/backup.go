package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 账号备份 / 迁移。
//
// 与 cockpit 导入的区别：这里是**本仓自己的**自包含快照，能原样搬走一条凭据
// （含备注、realm、企业 id、device token），导入时按 uid 精确合并，且不会像
// cockpit 那样对已有账号做上游求证（换 refresh token 会打断本机活跃号）。
//
// 加密刻意放在浏览器侧：口令与明文都不经过网关，导出接口只负责把凭据交给已鉴权
// 的前端，导入接口只收已解密的 JSON。带 encrypted 标记的信封直接 400，避免有人
// 以为服务端会解。

const (
	backupKind    = "workbuddy2api-account-backup"
	backupVersion = 1
	// backupNote 写进导出文件：说清哪些东西**不在**里面，避免被当成整机迁移包。
	backupNote = "只含账号凭据与备注，不含本机配置、模型黑名单、保留积分阈值、排程与告警设置。"
)

// backupAccount 快照里的单条账号。字段口径与 auth.Auth 对齐，但必须是独立 DTO：
// auth.Auth 没有 json tag 且 realm 未导出，直接 marshal 会漏字段。
type backupAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Realm        string `json:"realm,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // Unix 秒
	Domain       string `json:"domain,omitempty"`
	// Remark 由浏览器在下载前从 localStorage 合并进来；服务端不存备注，导入时忽略。
	Remark string `json:"remark,omitempty"`
}

type backupSnapshot struct {
	Kind       string          `json:"kind"`
	Version    int             `json:"version"`
	ExportedAt string          `json:"exported_at,omitempty"`
	Note       string          `json:"note,omitempty"`
	Accounts   []backupAccount `json:"accounts"`
	// Encrypted 只用于在导入接口上明确拒绝：加密由浏览器完成，网关不解密。
	Encrypted bool `json:"encrypted,omitempty"`
}

// exportAccounts 导出所选账号的自包含快照。
//
//	GET /panel/api/accounts/export?uids=<uid>,<uid>
//
// 返回 {"ok":true,"snapshot":{…},"missing":["…"]}。响应带 no-store：这份数据是
// 明文凭据，不能让浏览器或中间层缓存。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("uids"))
	if raw == "" {
		writeErr(w, http.StatusBadRequest, "uids required")
		return
	}
	seen := map[string]bool{}
	var list []backupAccount
	var missing []string
	for _, uid := range strings.Split(raw, ",") {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			missing = append(missing, uid)
			continue
		}
		list = append(list, snapshotOf(a))
	}
	if len(list) == 0 {
		writeErr(w, http.StatusNotFound, "no matching account")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"snapshot": backupSnapshot{
			Kind:       backupKind,
			Version:    backupVersion,
			ExportedAt: time.Now().Format(time.RFC3339),
			Note:       backupNote,
			Accounts:   list,
		},
		"missing": missing,
	})
}

// snapshotOf 在账号锁内拷一份凭据，避免与 RefreshToken 的写回撕开半个 token。
func snapshotOf(a *auth.Auth) backupAccount {
	// Realm() 自己会加锁，必须在拿锁之前调用，否则自锁。
	realm := a.Realm()
	a.Lock()
	defer a.Unlock()
	return backupAccount{
		UID:          a.UID,
		Nickname:     a.Nickname,
		Realm:        realm,
		EnterpriseID: a.EnterpriseID,
		DeviceToken:  a.DeviceToken,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
	}
}

// importAccounts 导入备份快照。
//
//	POST /panel/api/accounts/import?mode=merge|replace
//	Body: 已解密的快照 JSON（加密信封一律 400，解密在浏览器侧做）
//
// 两种模式都是白名单，未知值 400，不静默回落：
//   - merge   只增加。池中已有的 uid 一个字节都不写——从别人的老备份导入不会用
//     陈旧 token 打断本机活跃号。
//   - replace 覆盖已存在 / 新增。快照外的账号一律保留（刻意不提供「整体替换」，
//     删凭据不可逆）。
//
// 判重三层：池内 uid → 文件内重复 uid → 规范化（trim + 小写）后重复。
func (p *Panel) importAccounts(w http.ResponseWriter, r *http.Request) {
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode != "merge" && mode != "replace" {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("mode must be merge or replace, got %q", mode))
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var snap backupSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		writeErr(w, http.StatusBadRequest, "parse snapshot: "+err.Error())
		return
	}
	if snap.Encrypted {
		writeErr(w, http.StatusBadRequest,
			"encrypted backup must be decrypted in the browser before import")
		return
	}
	if snap.Kind != backupKind {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("not a %s file (kind=%q)", backupKind, snap.Kind))
		return
	}
	if snap.Version > backupVersion {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("unsupported snapshot version %d (max %d)", snap.Version, backupVersion))
		return
	}
	if len(snap.Accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "snapshot has no accounts")
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}

	var (
		created, updated, skipped int
		errs                      []string
	)
	seenUID := map[string]bool{}
	seenNorm := map[string]string{} // 规范化 uid -> 首次出现的 uid

	// 与登录落盘同一条锁序：写文件 + Pool.Add 期间不让目录轮询插进来。
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	for _, acc := range snap.Accounts {
		uid := strings.TrimSpace(acc.UID)
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid %q", acc.UID))
			continue
		}
		if seenUID[uid] {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: duplicate uid in file", uid))
			continue
		}
		norm := strings.ToLower(uid)
		if first, dup := seenNorm[norm]; dup {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: duplicates %s after normalization", uid, first))
			continue
		}
		seenUID[uid] = true
		seenNorm[norm] = uid

		at := strings.TrimSpace(acc.AccessToken)
		if at == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: missing access_token", uid))
			continue
		}
		rt := strings.TrimSpace(acc.RefreshToken)
		// 静态 API Key 账号本来就没有 refresh token；其余缺 RT 的凭据撑不过下一次刷新。
		if rt == "" && !(&auth.Auth{AccessToken: at}).IsAPIKey() {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: missing refresh_token", uid))
			continue
		}

		existing := p.cfg.Pool.AuthByUID(uid)
		if existing != nil && mode == "merge" {
			// merge 的全部意义：池里已有的号一个字节都不动。
			skipped++
			continue
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    backupExpiresAt(acc.ExpiresAt),
			Domain:       strings.TrimSpace(acc.Domain),
			UID:          uid,
			EnterpriseID: strings.TrimSpace(acc.EnterpriseID),
			Nickname:     strings.TrimSpace(acc.Nickname),
			DeviceToken:  strings.TrimSpace(acc.DeviceToken),
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}
		switch realm := strings.ToLower(strings.TrimSpace(acc.Realm)); realm {
		case "cn", "global":
			// 用显式 realm：只按 domain 推断会把「域看着是国际、实际是国内站」的号判错。
			if _, err := auth.BackfillRealmFor(a, realm); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("%s: %v", uid, err))
				continue
			}
		default:
			_, _ = a.BackfillRealm()
		}
		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("%s: save auth failed: %v", uid, err))
			continue
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)
		if existing != nil {
			updated++
		} else {
			created++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"mode":    mode,
		"total":   len(snap.Accounts),
		"created": created,
		"updated": updated,
		"skipped": skipped,
		"errors":  errs,
	})
}

// backupExpiresAt 归一化到期时间：快照写的是秒，容忍手改成毫秒或留空。
// 留空时给一年，与 cockpit 导入同口径——凭据本来就会在首次刷新时校正。
func backupExpiresAt(v int64) int64 {
	if v <= 0 {
		return time.Now().Add(365 * 24 * time.Hour).Unix()
	}
	if secs := normalizeExpiresAtMillis(v) / 1000; secs > 0 {
		return secs
	}
	return time.Now().Add(365 * 24 * time.Hour).Unix()
}
