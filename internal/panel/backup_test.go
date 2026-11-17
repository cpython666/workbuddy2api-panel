package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// 备份 / 迁移的测试夹具：目录 + 空池，上游一个端点都不配（导入路径不该发请求）。
type backupEnv struct {
	panel *Panel
	dir   string
}

func newBackupEnv(t *testing.T) *backupEnv {
	t.Helper()
	e := &backupEnv{dir: t.TempDir()}
	e.panel = New(Config{Pool: pool.New(""), AuthDir: e.dir, APIKey: "k"})
	return e
}

// seed 造一个池内已有的账号（同时落盘），返回其文件路径。
func (e *backupEnv) seed(t *testing.T, uid, at, rt string) string {
	t.Helper()
	a := &auth.Auth{
		UID: uid, AccessToken: at, RefreshToken: rt,
		ExpiresAt: 4102444800, Domain: "www.workbuddy.cn",
		Nickname: "nick-" + uid,
		FilePath: filepath.Join(e.dir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	e.panel.cfg.Pool.Add(a)
	return a.FilePath
}

func (e *backupEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (e *backupEnv) post(t *testing.T, query, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/panel/api/accounts/import"+query, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type importAccountsResp struct {
	OK      bool     `json:"ok"`
	Mode    string   `json:"mode"`
	Total   int      `json:"total"`
	Created int      `json:"created"`
	Updated int      `json:"updated"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors"`
}

func decodeImport(t *testing.T, body string) importAccountsResp {
	t.Helper()
	var r importAccountsResp
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return r
}

func snapshotJSON(t *testing.T, accounts ...backupAccount) string {
	t.Helper()
	raw, err := json.Marshal(backupSnapshot{
		Kind: backupKind, Version: backupVersion, Accounts: accounts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sampleAccount(uid, at, rt string) backupAccount {
	return backupAccount{
		UID: uid, AccessToken: at, RefreshToken: rt,
		Realm: "cn", Domain: "www.workbuddy.cn", Nickname: "nick-" + uid, ExpiresAt: 4102444800,
	}
}

// TestExportSnapshotCarriesCredentials：导出必须带齐搬号需要的字段，且响应不可缓存。
func TestExportSnapshotCarriesCredentials(t *testing.T) {
	e := newBackupEnv(t)
	e.seed(t, "uid-alice", "at-alice", "rt-alice")

	code, body := e.get(t, "/panel/api/accounts/export?uids=uid-alice,uid-ghost")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, body)
	}
	var resp struct {
		OK       bool           `json:"ok"`
		Snapshot backupSnapshot `json:"snapshot"`
		Missing  []string       `json:"missing"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !resp.OK || resp.Snapshot.Kind != backupKind || resp.Snapshot.Version != backupVersion {
		t.Fatalf("bad envelope: %+v", resp.Snapshot)
	}
	if resp.Snapshot.Note == "" {
		t.Fatal("note missing: 备份不含什么必须写进文件")
	}
	if len(resp.Snapshot.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(resp.Snapshot.Accounts))
	}
	got := resp.Snapshot.Accounts[0]
	if got.UID != "uid-alice" || got.AccessToken != "at-alice" || got.RefreshToken != "rt-alice" {
		t.Fatalf("credentials lost: %+v", got)
	}
	if got.Realm != "cn" || got.Nickname != "nick-uid-alice" || got.ExpiresAt != 4102444800 {
		t.Fatalf("metadata lost: %+v", got)
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != "uid-ghost" {
		t.Fatalf("missing=%v", resp.Missing)
	}
}

// TestImportMergeNeverTouchesExisting：merge 模式下池中已有账号的文件必须一个字节都不变。
func TestImportMergeNeverTouchesExisting(t *testing.T) {
	e := newBackupEnv(t)
	path := e.seed(t, "uid-alice", "at-alice", "rt-alice")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	body := snapshotJSON(t,
		sampleAccount("uid-alice", "at-stale", "rt-stale"),
		sampleAccount("uid-bob", "at-bob", "rt-bob"),
	)
	code, resp := e.post(t, "?mode=merge", body)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	r := decodeImport(t, resp)
	if r.Created != 1 || r.Updated != 0 || r.Skipped != 1 {
		t.Fatalf("counts: %+v", r)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("merge 改了已存在账号的凭据")
	}
	if e.panel.cfg.Pool.AuthByUID("uid-alice").AccessTokenValue() != "at-alice" {
		t.Fatal("merge 覆盖了池内活跃 token")
	}
	if e.panel.cfg.Pool.AuthByUID("uid-bob") == nil {
		t.Fatal("新账号没有进池")
	}
}

// TestImportReplaceKeepsOthers：replace 覆盖所选账号，快照外的账号必须原样保留。
func TestImportReplaceKeepsOthers(t *testing.T) {
	e := newBackupEnv(t)
	e.seed(t, "uid-alice", "at-alice", "rt-alice")
	e.seed(t, "uid-carol", "at-carol", "rt-carol")

	code, resp := e.post(t, "?mode=replace", snapshotJSON(t, sampleAccount("uid-alice", "at-new", "rt-new")))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	r := decodeImport(t, resp)
	if r.Updated != 1 || r.Created != 0 {
		t.Fatalf("counts: %+v", r)
	}
	if got := e.panel.cfg.Pool.AuthByUID("uid-alice").AccessTokenValue(); got != "at-new" {
		t.Fatalf("replace 没写入: %q", got)
	}
	if e.panel.cfg.Pool.AuthByUID("uid-carol") == nil {
		t.Fatal("replace 删掉了快照外的账号")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "workbuddy-uid-carol.json")); err != nil {
		t.Fatalf("快照外账号文件被动了: %v", err)
	}
}

// TestImportDedupeLayers：文件内重复 uid、规范化后重复都要跳过。
func TestImportDedupeLayers(t *testing.T) {
	e := newBackupEnv(t)
	body := snapshotJSON(t,
		sampleAccount("UID-Dup", "at-1", "rt-1"),
		sampleAccount("UID-Dup", "at-2", "rt-2"),
		sampleAccount("uid-dup", "at-3", "rt-3"),
	)
	code, resp := e.post(t, "?mode=merge", body)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	r := decodeImport(t, resp)
	if r.Total != 3 || r.Created != 1 || r.Skipped != 2 {
		t.Fatalf("counts: %+v", r)
	}
	if len(r.Errors) != 2 {
		t.Fatalf("errors=%v", r.Errors)
	}
}

// TestImportRejectsUnknownModeAndEnvelopes：白名单之外一律 400，不静默回落。
func TestImportRejectsUnknownModeAndEnvelopes(t *testing.T) {
	e := newBackupEnv(t)
	cases := []struct {
		name  string
		query string
		body  string
	}{
		{"unknown mode", "?mode=all", snapshotJSON(t, sampleAccount("uid-a", "at", "rt"))},
		{"empty mode", "", snapshotJSON(t, sampleAccount("uid-a", "at", "rt"))},
		{"encrypted", "?mode=merge", `{"kind":"` + backupKind + `","version":1,"encrypted":true,"accounts":[]}`},
		{"wrong kind", "?mode=merge", `{"kind":"cockpit","version":1,"accounts":[]}`},
		{"future version", "?mode=merge", `{"kind":"` + backupKind + `","version":99,"accounts":[]}`},
		{"no accounts", "?mode=merge", `{"kind":"` + backupKind + `","version":1,"accounts":[]}`},
	}
	for _, tc := range cases {
		code, body := e.post(t, tc.query, tc.body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.name, code, body)
		}
	}
}

// TestImportSkipsBrokenAccounts：坏条目跳过并说明原因，好条目照常写入。
func TestImportSkipsBrokenAccounts(t *testing.T) {
	e := newBackupEnv(t)
	noRT := sampleAccount("uid-nort", "at-nort", "")
	trav := backupAccount{UID: "../evil", AccessToken: "at", RefreshToken: "rt"}
	apiKey := backupAccount{UID: "uid-key", AccessToken: "ck_123", Realm: "cn"}

	code, resp := e.post(t, "?mode=merge", snapshotJSON(t, noRT, trav, apiKey))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	r := decodeImport(t, resp)
	if r.Created != 1 || r.Skipped != 2 {
		t.Fatalf("counts: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "workbuddy-uid-key.json")); err != nil {
		t.Fatalf("静态 API Key 账号应可导入: %v", err)
	}
}

// TestImportAPIKeyWithoutRefreshToken：ck_ 前缀账号没有 RT 是正常的，不能因此拒绝。
func TestImportAPIKeyWithoutRefreshToken(t *testing.T) {
	e := newBackupEnv(t)
	code, resp := e.post(t, "?mode=merge", snapshotJSON(t,
		backupAccount{UID: "uid-key", AccessToken: "ck_abc", Realm: "cn"}))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	if r := decodeImport(t, resp); r.Created != 1 || r.Skipped != 0 {
		t.Fatalf("counts: %+v", r)
	}
	got, err := auth.Parse(mustRead(t, filepath.Join(e.dir, "workbuddy-uid-key.json")))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsAPIKey() {
		t.Fatal("API Key 账号落盘后丢了 ck_ 前缀")
	}
}

// TestImportExplicitRealmWins：显式 realm 必须压过 domain 推断，否则国际域的国内号会被判错。
func TestImportExplicitRealmWins(t *testing.T) {
	e := newBackupEnv(t)
	acc := sampleAccount("uid-x", "at", "rt")
	acc.Realm = "global"
	acc.Domain = "www.workbuddy.ai"
	if code, resp := e.post(t, "?mode=merge", snapshotJSON(t, acc)); code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	if got := e.panel.cfg.Pool.AuthByUID("uid-x").Realm(); got != "global" {
		t.Fatalf("realm=%q", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
