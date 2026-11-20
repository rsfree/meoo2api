// Package store 负责状态的原子持久化、目录锁与自动保存。
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/model"
)

// Store 持有全量状态。所有字段读写必须在 Mu 保护下进行。
type Store struct {
	Mu   sync.RWMutex
	Data *model.Data

	dir      string
	file     string
	lockFile *os.File
	dirty    bool
	lastErr  string
}

// ErrLocked 表示数据目录已被另一个进程占用。
var ErrLocked = errors.New("数据目录已被其他进程占用")

// Load 打开数据目录并加载状态；缺失时生成新的状态与随机管理密码。
func Load(dir string) (*Store, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	s := &Store{dir: dir, file: filepath.Join(dir, "data.json")}
	lf, err := acquireLock(filepath.Join(dir, "data.lock"))
	if err != nil {
		return nil, "", err
	}
	s.lockFile = lf

	raw, err := os.ReadFile(s.file)
	generated := ""
	if errors.Is(err, os.ErrNotExist) {
		pw, derr := randomPassword()
		if derr != nil {
			return nil, "", derr
		}
		generated = pw
		s.Data = &model.Data{Version: 1, Config: model.DefaultConfig(), UpdatedAt: model.NowMilli()}
		s.Data.Config.AdminPass = HashPassword(pw)
		if err := s.Save(); err != nil {
			return nil, "", err
		}
		return s, generated, nil
	}
	if err != nil {
		return nil, "", err
	}
	var d model.Data
	if err := json.Unmarshal(raw, &d); err != nil {
		// 保留损坏文件原样，绝不自动清空账号。
		return nil, "", fmt.Errorf("状态文件损坏（已保留原文件 %s）: %w", s.file, err)
	}
	normalize(&d)
	s.Data = &d
	return s, "", nil
}

// normalize 补齐旧版本缺失字段。
func normalize(d *model.Data) {
	if d.Version == 0 {
		d.Version = 1
	}
	def := model.DefaultConfig()
	if d.Config.RequestTimeoutSec <= 0 {
		d.Config.RequestTimeoutSec = def.RequestTimeoutSec
	}
	if d.Config.QuotaCooldownSec <= 0 {
		d.Config.QuotaCooldownSec = def.QuotaCooldownSec
	}
	if d.Config.RateCooldownSec <= 0 {
		d.Config.RateCooldownSec = def.RateCooldownSec
	}
	if d.Config.KeepaliveSec < 0 {
		d.Config.KeepaliveSec = def.KeepaliveSec
	}
	if d.Config.Aliases == nil {
		d.Config.Aliases = map[string]string{}
	}
}

// Close 释放目录锁。
func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	err := releaseLock(s.lockFile)
	s.lockFile = nil
	return err
}

// Touch 标记有未保存变更。
func (s *Store) Touch() {
	s.Mu.Lock()
	s.dirty = true
	s.Data.UpdatedAt = model.NowMilli()
	s.Mu.Unlock()
}

// Save 原子写入：临时文件 → fsync → rename，权限 0600。
func (s *Store) Save() error {
	s.Mu.RLock()
	if s.Data == nil {
		s.Mu.RUnlock()
		return errors.New("状态未初始化")
	}
	buf, err := json.MarshalIndent(s.Data, "", " ")
	s.Mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(buf); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, s.file); err != nil {
		return err
	}
	if d, derr := os.Open(s.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.Mu.Lock()
	s.dirty = false
	s.lastErr = ""
	s.Mu.Unlock()
	return nil
}

// SaveLoop 每 3 秒落盘一次脏状态，直到 ctx 结束。
func (s *Store) SaveLoop(done <-chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			_ = s.Save()
			return
		case <-t.C:
			s.Mu.RLock()
			dirty := s.dirty
			s.Mu.RUnlock()
			if dirty {
				if err := s.Save(); err != nil {
					s.Mu.Lock()
					s.lastErr = err.Error()
					s.Mu.Unlock()
				}
			}
		}
	}
}

// LastError 返回最近一次落盘失败原因（用于面板告警）。
func (s *Store) LastError() string {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.lastErr
}

// ---- 密码 ----

// HashPassword 生成 pbkdf2-sha256 编码串（迭代 21 万次）。
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		copy(salt, []byte(fmt.Sprint(time.Now().UnixNano())))
	}
	dk := pbkdf2SHA256([]byte(pw), salt, 210000, 32)
	return fmt.Sprintf("pbkdf2$210000$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
}

// CheckPassword 校验密码；返回 (是否通过, 是否为旧格式需要升级)。
func CheckPassword(pw, encoded string) (bool, bool) {
	if strings.HasPrefix(encoded, "pbkdf2$") {
		parts := strings.Split(encoded, "$")
		if len(parts) != 4 {
			return false, false
		}
		var iter int
		if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter < 1000 {
			return false, false
		}
		salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
		want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
		if err1 != nil || err2 != nil {
			return false, false
		}
		got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
		return subtle.ConstantTimeCompare(got, want) == 1, false
	}
	// 兼容旧版裸 sha256：登录成功后由调用方升级。
	sum := sha256.Sum256([]byte(pw))
	if subtle.ConstantTimeCompare([]byte(fmt.Sprintf("%x", sum)), []byte(encoded)) == 1 {
		return true, true
	}
	return false, false
}

func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 去掉易混淆字符，便于手抄。
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]byte, 16)
	for i := range out {
		out[i] = alphabet[int(b[i%len(b)])%len(alphabet)]
	}
	return string(out), nil
}

// pbkdf2SHA256 是 RFC 2898 的最小实现，避免引入外部依赖。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := func(data []byte) []byte {
		h := sha256.New()
		h.Write(data)
		return h.Sum(nil)
	}
	hashLen := sha256.Size
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var out []byte
	for block := 1; block <= numBlocks; block++ {
		u := prf(append(append(append([]byte{}, password...), salt...), byte(block>>24), byte(block>>16), byte(block>>8), byte(block)))
		t := make([]byte, len(u))
		copy(t, u)
		for n := 2; n <= iter; n++ {
			u = prf(u)
			for i := range t {
				t[i] ^= u[i]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
