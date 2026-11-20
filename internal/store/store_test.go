package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"meoo2api/internal/model"
)

func TestPasswordRoundTrip(t *testing.T) {
	enc := HashPassword("hunter2hunter2")
	if ok, legacy := CheckPassword("hunter2hunter2", enc); !ok || legacy {
		t.Fatalf("正确密码应通过且非旧格式，得到 ok=%v legacy=%v", ok, legacy)
	}
	if ok, _ := CheckPassword("wrong", enc); ok {
		t.Fatal("错误密码必须拒绝")
	}
	// 同一密码两次哈希必须不同（随机盐）
	if HashPassword("abc") == HashPassword("abc") {
		t.Fatal("盐没有随机化")
	}
}

func TestLegacySHA256UpgradeSignal(t *testing.T) {
	// 旧格式：裸 sha256 hex，登录成功时调用方负责升级
	legacyEnc := sha256Hex("oldpass")
	ok, legacy := CheckPassword("oldpass", legacyEnc)
	if !ok || !legacy {
		t.Fatalf("旧格式应通过并标记需要升级，得到 ok=%v legacy=%v", ok, legacy)
	}
}

func TestLoadCreatesAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, pw, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if pw == "" {
		t.Fatal("首启应返回一次性密码")
	}
	s.Mu.Lock()
	s.Data.Accounts = append(s.Data.Accounts, &model.Account{ID: "a_1", UserID: "u1", Enabled: true})
	s.Mu.Unlock()
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.Close()

	// 权限必须是 0600
	info, err := os.Stat(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("data.json 权限 = %o，期望 600", info.Mode().Perm())
	}

	s2, pw2, err := Load(dir)
	if err != nil {
		t.Fatalf("二次 Load: %v", err)
	}
	defer s2.Close()
	if pw2 != "" {
		t.Fatal("已有状态不应重新生成密码")
	}
	if len(s2.Data.Accounts) != 1 || s2.Data.Accounts[0].UserID != "u1" {
		t.Fatalf("账号没有正确恢复: %+v", s2.Data.Accounts)
	}
}

func TestCorruptedFileKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(dir); err == nil {
		t.Fatal("损坏的状态文件必须报错")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("损坏文件不应被删除")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{not json" {
		t.Fatal("损坏文件不应被改写")
	}
}

func TestLockExcludesSecondProcess(t *testing.T) {
	dir := t.TempDir()
	s, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := Load(dir); err == nil {
		t.Fatal("同一目录第二个实例必须被拒绝")
	}
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}
