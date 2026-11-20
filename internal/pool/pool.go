// Package pool 负责项目 AK 的选号、冷却与探活。
package pool

import (
	"context"
	"math/rand"
	"sort"
	"sync"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

// Picker 是无状态选号器：状态全部来自 Store，进程重启不丢冷却。
type Picker struct {
	St *store.Store
	Up *upstream.Client

	mu       sync.Mutex
	rnd      *rand.Rand
	last     map[string]int64 // keyID → 最近一次被选中时间
	inflight map[string]int   // keyID → 在途请求数（负载均衡 + 防惊群）
}

// Usable 返回当前可用凭证（供面板与 /healthz 使用）。
func (p *Picker) Usable() []*model.Key {
	now := model.NowMilli()
	p.St.Mu.RLock()
	defer p.St.Mu.RUnlock()
	out := make([]*model.Key, 0, len(p.St.Data.Keys))
	for _, k := range p.St.Data.Keys {
		if usableLocked(p.St, k, now) {
			out = append(out, k)
		}
	}
	return out
}

func usableLocked(st *store.Store, k *model.Key, now int64) bool {
	if k == nil || !k.Enabled {
		return false
	}
	if k.Health.State == "invalid" {
		return false
	}
	if k.Health.Until > now {
		return false
	}
	// 账号被停用/暂停时，其全部项目 AK 一并排除。
	for _, a := range st.Data.Accounts {
		if a.ID == k.AccountID {
			if !a.Enabled || a.Paused {
				return false
			}
			return true
		}
	}
	// 未绑定账号的历史凭证仍然可用（不受账号启停影响）。
	return true
}

// Pick 选出一个凭证。exclude 是本请求已经尝试过的 keyID。
func (p *Picker) Pick(exclude map[string]bool) *model.Key {
	now := model.NowMilli()
	p.St.Mu.RLock()
	type cand struct {
		k     *model.Key
		score float64
	}
	var cands []cand
	for _, k := range p.St.Data.Keys {
		if exclude[k.ID] || !usableLocked(p.St, k, now) {
			continue
		}
		score := 1.0
		// 积分越高的账号权重越大（同账号多 key 时自然分摊）。
		for _, a := range p.St.Data.Accounts {
			if a.ID == k.AccountID {
				if a.Points > 0 {
					score += float64(a.Points) / 1000
				}
				break
			}
		}
		cands = append(cands, cand{k: k, score: score})
	}
	p.St.Mu.RUnlock()
	if len(cands) == 0 {
		return nil
	}
	p.mu.Lock()
	if p.rnd == nil {
		p.rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
		p.last = map[string]int64{}
		p.inflight = map[string]int{}
	}
	// 在途最少的优先（并发时天然分摊），其次最久未被选中的优先。
	sort.SliceStable(cands, func(i, j int) bool {
		li, lj := p.inflight[cands[i].k.ID], p.inflight[cands[j].k.ID]
		if li != lj {
			return li < lj
		}
		return p.last[cands[i].k.ID] < p.last[cands[j].k.ID]
	})
	// 取最久未用的前一半做加权随机。
	n := len(cands)
	if n > 3 {
		n = (n + 1) / 2
	}
	pool := cands[:n]
	total := 0.0
	for _, c := range pool {
		total += c.score
	}
	r := p.rnd.Float64() * total
	chosen := pool[len(pool)-1].k
	for _, c := range pool {
		r -= c.score
		if r <= 0 {
			chosen = c.k
			break
		}
	}
	p.last[chosen.ID] = now
	p.mu.Unlock()
	return chosen
}

// PickLeased 选号并占用一个在途名额；调用方必须在请求结束后 Release。
// 与 Pick 分开，是为了让目录同步、面板探活这类短调用不占用并发配额。
func (p *Picker) PickLeased(exclude map[string]bool) *model.Key {
	k := p.Pick(exclude)
	if k == nil {
		return nil
	}
	p.mu.Lock()
	if p.inflight == nil {
		p.inflight = map[string]int{}
	}
	p.inflight[k.ID]++
	p.mu.Unlock()
	return k
}

// Done 归还在途名额。
func (p *Picker) Done(keyID string) {
	p.mu.Lock()
	if p.inflight != nil && p.inflight[keyID] > 0 {
		p.inflight[keyID]--
	}
	p.mu.Unlock()
}

// Inflight 返回某凭证当前在途请求数（面板观测用）。
func (p *Picker) Inflight(keyID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight[keyID]
}

// MarkOK 清除失败计数并更新使用时间，同时按模型累计 token（用于反推单价）。
func (p *Picker) MarkOK(keyID, modelName string, tokens int64) {
	now := model.NowMilli()
	p.St.Mu.Lock()
	accountID := ""
	for _, k := range p.St.Data.Keys {
		if k.ID == keyID {
			k.Health = model.Health{State: "ok"}
			k.LastUsed = now
			k.Requests++
			k.Tokens += tokens
			accountID = k.AccountID
			break
		}
	}
	if accountID != "" && modelName != "" && tokens > 0 {
		for _, a := range p.St.Data.Accounts {
			if a.ID == accountID {
				if a.Usage == nil {
					a.Usage = map[string]int64{}
				}
				a.Usage[modelName] += tokens
				break
			}
		}
	}
	p.St.Mu.Unlock()
	p.St.Touch()
}

// Cooldown 按错误种类处置凭证：
//   - quota  → 长冷却（默认 30 分钟）
//   - rate   → 短冷却（默认 10 分钟，指数退避封顶 1 小时）
//   - auth   → 直接标记 invalid，需人工处理或探活恢复
//   - server → 短冷却 60s
//
// 返回冷却截止毫秒时间戳（0 表示未冷却）。
func (p *Picker) Cooldown(keyID string, ue *upstream.Error) (state string, until int64) {
	now := model.NowMilli()
	p.St.Mu.RLock()
	cfg := p.St.Data.Config
	p.St.Mu.RUnlock()
	switch ue.Kind {
	case upstream.KindQuota:
		until = now + int64(cfg.QuotaCooldownSec)*1000
		state = "quota"
	case upstream.KindRate:
		sec := cfg.RateCooldownSec
		if sec <= 0 {
			sec = 600
		}
		until = now + int64(sec)*1000
		state = "cooldown"
	case upstream.KindAuth:
		state = "invalid"
	case upstream.KindServer:
		until = now + 60_000
		state = "cooldown"
	default:
		return "", 0
	}
	p.St.Mu.Lock()
	for _, k := range p.St.Data.Keys {
		if k.ID == keyID {
			k.Fails++
			k.Health = model.Health{State: state, Reason: ue.Code + " " + ue.Message, Until: until, Failures: int(k.Fails)}
			break
		}
	}
	p.St.Mu.Unlock()
	p.St.Touch()
	return state, until
}

// Release 人工解除冷却。
func (p *Picker) Release(keyID string) {
	p.St.Mu.Lock()
	for _, k := range p.St.Data.Keys {
		if k.ID == keyID {
			k.Health = model.Health{State: "ok"}
			break
		}
	}
	p.St.Mu.Unlock()
	p.St.Touch()
}

// Probe 以 /models 探活：200 视为可用。不把 5xx/超时判死。
func (p *Picker) Probe(ctx context.Context, k *model.Key) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, _, err := p.Up.DoJSON(ctx, "GET", p.Up.Base+"/models", nil, upstream.MergeHeader(k.Key, nil))
	if err == nil && code == 200 {
		p.MarkOK(k.ID, "", 0)
		return true, "ok"
	}
	if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.KindAuth {
		p.Cooldown(k.ID, ue)
		return false, ue.Error()
	}
	if err != nil {
		return false, err.Error()
	}
	return false, "HTTP 非 200"
}
