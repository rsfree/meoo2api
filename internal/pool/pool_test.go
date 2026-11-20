package pool

import (
	"testing"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, _, err := store.Load(t.TempDir())
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(st *store.Store, accEnabled bool, keyEnabled bool, health model.Health) (*model.Account, *model.Key) {
	a := &model.Account{ID: "a1", UserID: "u1", Enabled: accEnabled, Points: 100}
	k := &model.Key{ID: "k1", Key: "meoo_project_x", AccountID: "a1", Enabled: keyEnabled, Health: health}
	st.Mu.Lock()
	st.Data.Accounts = append(st.Data.Accounts, a)
	st.Data.Keys = append(st.Data.Keys, k)
	st.Mu.Unlock()
	return a, k
}

func TestPickSkipsDisabledAndCooling(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "cooldown", Until: model.NowMilli() + 60000})
	p := &Picker{St: st}
	if got := p.Pick(map[string]bool{}); got != nil {
		t.Fatalf("冷却中的凭证不应被选中: %+v", got)
	}
	if n := len(p.Usable()); n != 0 {
		t.Fatalf("Usable 应为 0，得到 %d", n)
	}

	// 冷却到期后可再次选中
	st.Mu.Lock()
	st.Data.Keys[0].Health = model.Health{State: "cooldown", Until: model.NowMilli() - 1}
	st.Mu.Unlock()
	if got := p.Pick(map[string]bool{}); got == nil {
		t.Fatal("冷却过期后应可选")
	}
}

func TestPickExcludesTried(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	p := &Picker{St: st}
	if got := p.Pick(map[string]bool{"k1": true}); got != nil {
		t.Fatalf("已尝试过的凭证不应重复返回: %+v", got)
	}
}

func TestPickRespectsAccountState(t *testing.T) {
	for _, c := range []struct {
		name    string
		enabled bool
		paused  bool
		want    bool
	}{
		{"正常", true, false, true},
		{"停用", false, false, false},
		{"暂停", true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(t)
			a, _ := seed(st, c.enabled, true, model.Health{State: "ok"})
			a.Paused = c.paused
			p := &Picker{St: st}
			got := p.Pick(map[string]bool{})
			if (got != nil) != c.want {
				t.Fatalf("选中=%v，期望 %v", got != nil, c.want)
			}
		})
	}
}

func TestCooldownByKind(t *testing.T) {
	now := model.NowMilli()
	cases := []struct {
		kind  string
		state string
		minMs int64
	}{
		{upstream.KindQuota, "quota", 1800 * 1000},
		{upstream.KindRate, "cooldown", 600 * 1000},
		{upstream.KindServer, "cooldown", 60 * 1000},
		{upstream.KindAuth, "invalid", 0},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			st := newStore(t)
			seed(st, true, true, model.Health{State: "ok"})
			p := &Picker{St: st}
			state, until := p.Cooldown("k1", &upstream.Error{Kind: c.kind, Message: "x"})
			if state != c.state {
				t.Fatalf("state = %q，期望 %q", state, c.state)
			}
			if c.minMs > 0 && until-now < c.minMs {
				t.Fatalf("冷却时长 %dms 少于期望 %dms", until-now, c.minMs)
			}
			// 冷却后不可选
			if got := p.Pick(map[string]bool{}); got != nil {
				t.Fatalf("%s 冷却后不应可选", c.kind)
			}
		})
	}
}

func TestCooldownIgnoredForBadRequest(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	p := &Picker{St: st}
	if state, _ := p.Cooldown("k1", &upstream.Error{Kind: upstream.KindBadRequest}); state != "" {
		t.Fatalf("参数错误不应冷却凭证，得到 %q", state)
	}
	if p.Pick(map[string]bool{}) == nil {
		t.Fatal("参数错误后凭证仍应可用")
	}
}

func TestReleaseClearsCooldown(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "quota", Until: model.NowMilli() + 999999})
	p := &Picker{St: st}
	if p.Pick(map[string]bool{}) != nil {
		t.Fatal("冷却中不应可选")
	}
	p.Release("k1")
	if p.Pick(map[string]bool{}) == nil {
		t.Fatal("解除冷却后应可选")
	}
}

func TestMarkOKAccumulatesUsage(t *testing.T) {
	st := newStore(t)
	_, k := seed(st, true, true, model.Health{State: "cooldown", Until: model.NowMilli() + 1000})
	p := &Picker{St: st}
	p.MarkOK(k.ID, "qwen", 120)
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	if k.Health.State != "ok" || k.Requests != 1 || k.Tokens != 120 {
		t.Fatalf("MarkOK 状态错误: %+v", k)
	}
}
