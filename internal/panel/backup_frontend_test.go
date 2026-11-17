package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestBackupFrontendWiring 备份 / 迁移的界面接线：勾选 → 导出、选文件 → 导入，
// 加密只在浏览器里做（网关收到的一律是已解密的快照）。
func TestBackupFrontendWiring(t *testing.T) {
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	h := string(html)
	for _, want := range []string{
		`data-tab="backup"`,
		`id="addTabBackup"`,
		`id="backupPickList"`,
		`id="backupPass"`,
		`id="btnBackupExport"`,
		`id="backupFile"`,
		`name="backupMode" value="merge"`,
		`name="backupMode" value="replace"`,
		`id="backupPassIn"`,
		`id="btnBackupImport"`,
		`id="backupDone"`,
		`id="backupErr"`,
	} {
		if !strings.Contains(h, want) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	j := string(js)
	for _, want := range []string{
		"accounts/export?uids=",
		"accounts/import?mode=",
		"await encryptSnapshot(snap, pass)",
		"snap = await decryptSnapshot(snap, pass)",
		"if (snap && snap.encrypted)",
		// 加密信封绝不能直接 POST 上去：请求体必须是解密后的快照。
		`api('accounts/import?mode=' + encodeURIComponent(mode), { method: 'POST', body: JSON.stringify(snap) })`,
	} {
		if !strings.Contains(j, want) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}

	// 顺序：导出必须先取快照（含备注）再决定加不加密，不能先加密再补备注。
	getAt := strings.Index(j, "accounts/export?uids=")
	encAt := strings.Index(j, "await encryptSnapshot(snap, pass)")
	if getAt < 0 || encAt < 0 || encAt < getAt {
		t.Fatal("加密发生在取快照之前")
	}
	// 导入必须先生成快照变量再 POST，且加密分支在前。
	if strings.Index(j, "snap = await decryptSnapshot") > strings.Index(j, "body: JSON.stringify(snap)") {
		t.Fatal("解密发生在提交之后")
	}
}

// TestBackupCryptoRoundtrip 加密信封的往返与错口令拒绝，直接在 node 里跑真实
// WebCrypto（不是 mock），确保 PBKDF2/AES-GCM 参数能被同一个函数解密回来。
func TestBackupCryptoRoundtrip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node 不可用，跳过 WebCrypto 往返测试")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "const BACKUP_ITER")
	end := strings.Index(s, "function downloadText")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("切片锚点找不到")
	}
	snippet := s[start:end]

	harness := `
globalThis.crypto = require('node:crypto').webcrypto;
` + snippet + `
(async () => {
  const snap = {kind:'workbuddy2api-account-backup', version:1,
    accounts:[{uid:'u-1', access_token:'at-1', refresh_token:'rt-1'}]};
  const env = await encryptSnapshot(snap, 'p@ss-口令');
  const back = await decryptSnapshot(env, 'p@ss-口令');
  const wrong = await decryptSnapshot(env, 'bad').then(() => 'DECRYPTED', () => 'REJECTED');
  process.stdout.write(JSON.stringify({env, back, wrong}));
})();
`
	cmd := exec.Command(node, "-e", harness)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got struct {
		Env struct {
			Encrypted  bool   `json:"encrypted"`
			Ciphertext string `json:"ciphertext"`
			KDF        struct {
				Name       string `json:"name"`
				Hash       string `json:"hash"`
				Iterations int    `json:"iterations"`
				Salt       string `json:"salt"`
			} `json:"kdf"`
			Cipher struct {
				Name string `json:"name"`
				IV   string `json:"iv"`
			} `json:"cipher"`
		} `json:"env"`
		Back  backupSnapshot `json:"back"`
		Wrong string         `json:"wrong"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解码结果失败: %v (%s)", err, out)
	}
	if !got.Env.Encrypted || got.Env.Ciphertext == "" {
		t.Fatalf("信封不完整: %+v", got.Env)
	}
	if got.Env.KDF.Name != "PBKDF2" || got.Env.KDF.Hash != "SHA-256" || got.Env.KDF.Iterations != 200000 {
		t.Fatalf("KDF 参数不符: %+v", got.Env.KDF)
	}
	if got.Env.KDF.Salt == "" || got.Env.Cipher.IV == "" {
		t.Fatal("salt / iv 缺失")
	}
	if len(got.Back.Accounts) != 1 || got.Back.Accounts[0].AccessToken != "at-1" {
		t.Fatalf("往返丢了凭据: %+v", got.Back)
	}
	if got.Wrong != "REJECTED" {
		t.Fatalf("错口令没被拒绝: %s", got.Wrong)
	}
}
