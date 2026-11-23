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
	if got := p.Pick(map[string]bool{}, ""); got != nil {
		t.Fatalf("冷却中的凭证不应被选中: %+v", got)
	}
	if n := len(p.Usable()); n != 0 {
		t.Fatalf("Usable 应为 0，得到 %d", n)
	}

	// 冷却到期后可再次选中
	st.Mu.Lock()
	st.Data.Keys[0].Health = model.Health{State: "cooldown", Until: model.NowMilli() - 1}
	st.Mu.Unlock()
	if got := p.Pick(map[string]bool{}, ""); got == nil {
		t.Fatal("冷却过期后应可选")
	}
}

func TestPickExcludesTried(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	p := &Picker{St: st}
	if got := p.Pick(map[string]bool{"k1": true}, ""); got != nil {
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
			got := p.Pick(map[string]bool{}, "")
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
			if got := p.Pick(map[string]bool{}, ""); got != nil {
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
	if p.Pick(map[string]bool{}, "") == nil {
		t.Fatal("参数错误后凭证仍应可用")
	}
}

func TestReleaseClearsCooldown(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "quota", Until: model.NowMilli() + 999999})
	p := &Picker{St: st}
	if p.Pick(map[string]bool{}, "") != nil {
		t.Fatal("冷却中不应可选")
	}
	p.Release("k1")
	if p.Pick(map[string]bool{}, "") == nil {
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

func seedProvider(st *store.Store, id, provider string) *model.Key {
	k := &model.Key{ID: id, Key: "sk_" + id, AccountID: "a1", Provider: provider, Enabled: true, Health: model.Health{State: "ok"}}
	st.Mu.Lock()
	st.Data.Keys = append(st.Data.Keys, k)
	st.Mu.Unlock()
	return k
}

// 空字段的旧数据必须回退默认供应商——这是「旧 data.json 零迁移」的契约。
func TestProviderNameDefaultsToMeoo(t *testing.T) {
	k := &model.Key{ID: "k_old"}
	if k.ProviderName() != model.DefaultProvider {
		t.Fatalf("空 provider 应回退 %q，得到 %q", model.DefaultProvider, k.ProviderName())
	}
	k.Provider = " DeepSeek "
	if k.ProviderName() != "deepseek" {
		t.Fatalf("provider 应归一化为 deepseek，得到 %q", k.ProviderName())
	}
}

func TestPickFiltersByProvider(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	seedProvider(st, "k_ds", "deepseek")
	p := &Picker{St: st}

	if got := p.Pick(map[string]bool{}, "deepseek"); got == nil || got.ID != "k_ds" {
		t.Fatalf("按 deepseek 选号应得 k_ds，得到 %+v", got)
	}
	if got := p.Pick(map[string]bool{}, model.DefaultProvider); got == nil || got.ID != "k1" {
		t.Fatalf("按 meoo 选号应得 k1，得到 %+v", got)
	}
	// 该供应商无可用凭证时返回 nil，而不是把别家的号错配出去
	if got := p.Pick(map[string]bool{}, "openai"); got != nil {
		t.Fatalf("未注册供应商不应选到任何凭证: %+v", got)
	}
	// 空 provider = 不限，任何一家的都行
	if got := p.Pick(map[string]bool{}, ""); got == nil {
		t.Fatal("不限供应商时应有凭证可选")
	}
}

func TestClientForFallsBackToUp(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	up := upstream.New("https://meoo.example/v1")
	p := &Picker{St: st, Up: up} // Reg 未接线（旧构造路径）
	if got := p.ClientFor(st.Data.Keys[0]); got != up {
		t.Fatal("Reg 未接线时 ClientFor 应回退 Up")
	}
}

func TestClientForRoutesByProvider(t *testing.T) {
	st := newStore(t)
	seed(st, true, true, model.Health{State: "ok"})
	ds := seedProvider(st, "k_ds", "deepseek")
	reg := upstream.NewRegistry(model.DefaultProvider, "https://meoo.example/v1")
	reg.Add("deepseek", "https://ds.example/v1")
	p := &Picker{St: st, Up: reg.Get(model.DefaultProvider), Reg: reg}

	if got := p.ClientFor(ds); got.Base != "https://ds.example/v1" {
		t.Fatalf("deepseek 凭证应路由到 ds base，得到 %s", got.Base)
	}
	if got := p.ClientFor(st.Data.Keys[0]); got.Base != "https://meoo.example/v1" {
		t.Fatalf("meoo 凭证应路由到 meoo base，得到 %s", got.Base)
	}
}
