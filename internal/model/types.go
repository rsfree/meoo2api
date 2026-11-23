// Package model 定义网关的全部持久化数据结构与取值约束。
package model

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// 上游凭证前缀。三种凭证互相独立，不能混用：
//   - meoo_ak_…       个人 AK：身份、项目与云服务管理、查积分；不能直接调模型
//   - meoo_project_…  项目服务 AK：唯一能调模型网关的凭证，进账号池
//   - Cookie          仅用于网页签到与保活，可选
const (
	PersonalAKPrefix = "meoo_ak_"
	ProjectAKPrefix  = "meoo_project_"
	ClientKeyPrefix  = "sk-"
)

// Account 是一个 Meoo 身份。
type Account struct {
	ID           string `json:"id"`
	UserID       string `json:"user_id"`
	Name         string `json:"name"`
	Label        string `json:"label"`
	Enabled      bool   `json:"enabled"`
	Paused       bool   `json:"paused,omitempty"` // 人工暂停，不被自动任务解除
	CreatedAt    int64  `json:"created_at"`
	PersonalAK   string `json:"personal_ak,omitempty"`
	PersonalAKAt int64  `json:"personal_ak_at,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	Cookie       string `json:"cookie,omitempty"`

	// 观测字段（非权威，随时可被探活刷新）
	Points      float64 `json:"points"`
	PointsAt    int64   `json:"points_at,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	LastCheckAt int64   `json:"last_check_at,omitempty"`
	CheckOK     bool    `json:"check_ok,omitempty"`

	KeyIDs []string `json:"key_ids,omitempty"`

	// 每日签到：Meoo 没有手动签到按钮，「签到」= 用网页登录态领当天的登录赠送积分。
	// 只有 Cookie 能领（AK 被 API_KEY_ROUTE_FORBIDDEN 拒绝），所以签到结果必须落盘，
	// 否则面板刷新一次就忘了昨天领没领。
	CheckinAt     int64   `json:"checkin_at,omitempty"`     // 最近一次签到尝试（含失败）
	CheckinDate   string  `json:"checkin_date,omitempty"`   // 最近一次成功签到的日期（CST，YYYY-MM-DD）
	CheckinAmount float64 `json:"checkin_amount,omitempty"` // 该次领到的积分
	CheckinErr    string  `json:"checkin_err,omitempty"`    // 最近一次失败原因

	// 成本观测：按模型累计 token，配合积分快照反推真实单价。
	Usage         map[string]int64 `json:"usage,omitempty"`
	Spent         int64            `json:"spent,omitempty"`          // 累计积分消耗（采样得出）
	CostSamples   []CostSample     `json:"cost_samples,omitempty"`   // 最近若干次采样
	PointsBase    float64          `json:"points_base,omitempty"`    // 上次采样时的可用积分
	UsageSnapshot map[string]int64 `json:"usage_snapshot,omitempty"` // 上次采样时的累计用量
}

// CostSample 是一次"积分快照差分"：期间消耗了多少积分、各模型用了多少 token。
type CostSample struct {
	At     int64            `json:"at"`
	Delta  int64            `json:"delta"`
	Tokens map[string]int64 `json:"tokens"`
}

// Health 描述一个项目 AK 的可用状态。
type Health struct {
	State    string `json:"state"` // ok | cooldown | quota | invalid
	Reason   string `json:"reason,omitempty"`
	Until    int64  `json:"until,omitempty"`
	Failures int    `json:"failures,omitempty"`
}

// Key 是一个项目服务 AK（入池凭证）。
type Key struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	AccountID string `json:"account_id"`
	ProjectID string `json:"project_id"`
	Label     string `json:"label,omitempty"`
	Enabled   bool   `json:"enabled"`
	Health    Health `json:"health"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used,omitempty"`
	Requests  int64  `json:"requests"`
	Tokens    int64  `json:"tokens"`
	Fails     int64  `json:"fails"`
}

// ClientKey 是签发给客户端的 sk-…。
type ClientKey struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	Requests  int64  `json:"requests"`
	Tokens    int64  `json:"tokens"`
	LastUsed  int64  `json:"last_used,omitempty"`
	Budget    int64  `json:"budget,omitempty"` // 0 = 不限
}

// Config 是热生效的运行时配置。
type Config struct {
	DefaultMaxTokens  int               `json:"default_max_tokens"`  // 0 = 跟随上游
	RequestTimeoutSec int               `json:"request_timeout_sec"` // 默认 180
	QuotaCooldownSec  int               `json:"quota_cooldown_sec"`  // 余额不足冷却，默认 1800
	RateCooldownSec   int               `json:"rate_cooldown_sec"`   // 限流冷却，默认 600
	Failover          bool              `json:"failover"`            // 自动换号
	KeepaliveSec      int               `json:"keepalive_sec"`       // SSE 心跳间隔，0 关闭
	Aliases           map[string]string `json:"aliases,omitempty"`
	AliasNotes        map[string]string `json:"alias_notes,omitempty"` // 别名备注，仅面板展示用
	HiddenModels      []string          `json:"hidden_models,omitempty"`
	AdminPass         string            `json:"admin_pass,omitempty"` // pbkdf2 编码

	// 每日自动签到：到点后用各账号的网页 Cookie 领登录赠送积分。
	// 默认关闭——Cookie 有寿命（阿里 SSO ≤2 天），自动跑失败只会留一串错误。
	CheckinAuto bool   `json:"checkin_auto"`
	CheckinTime string `json:"checkin_time,omitempty"` // HH:MM，默认 09:00
}

// Data 是整个状态树的根。
type Data struct {
	Version    int          `json:"version"`
	Accounts   []*Account   `json:"accounts"`
	Keys       []*Key       `json:"keys"`
	ClientKeys []*ClientKey `json:"client_keys"`
	Config     Config       `json:"config"`
	UpdatedAt  int64        `json:"updated_at"`
}

// DefaultConfig 返回首启推荐配置。
func DefaultConfig() Config {
	return Config{
		DefaultMaxTokens:  0,
		RequestTimeoutSec: 180,
		QuotaCooldownSec:  1800,
		RateCooldownSec:   600,
		Failover:          true,
		KeepaliveSec:      12,
		Aliases:           map[string]string{"qwen": "qwen3.8-flash", "ds": "deepseek-v4-flash-0731"},
		CheckinTime:       "09:00",
	}
}

// RandID 生成 prefix + n 字节随机数的十六进制标识。
func RandID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return prefix + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	}
	return prefix + hex.EncodeToString(b)
}

// Mask 保留首尾用于展示，绝不回显完整凭证。
func Mask(s string) string {
	if len(s) <= 14 {
		return strings.Repeat("*", len(s))
	}
	return s[:10] + "…" + s[len(s)-4:]
}

// IsProjectAK 判断是否项目服务 AK 形态。
func IsProjectAK(s string) bool {
	return strings.HasPrefix(s, ProjectAKPrefix) && len(s) > len(ProjectAKPrefix)+8
}

// IsPersonalAK 判断是否个人 AK 形态。
func IsPersonalAK(s string) bool {
	return strings.HasPrefix(s, PersonalAKPrefix) && len(s) > len(PersonalAKPrefix)+8
}

// NowMilli 统一毫秒时间戳。
func NowMilli() int64 { return time.Now().UnixMilli() }
