package admin

import "testing"

func TestValidateClientKey(t *testing.T) {
	ok := []string{"sk-meoo2api", "sk-a1b2", "sk-abcDEF123_-."}
	for _, k := range ok {
		if err := ValidateClientKey(k); err != nil {
			t.Fatalf("%q 应通过，却报 %v", k, err)
		}
	}
	bad := []string{
		"meoo2api",       // 缺前缀
		"sk-",            // 太短
		"sk-has space",   // 空格
		"sk-中文",         // 非 ASCII
		"sk-with\\slash", // 反斜杠
		"sk-" + string(make([]byte, 70)),
	}
	for _, k := range bad {
		if err := ValidateClientKey(k); err == nil {
			t.Fatalf("%q 应被拒，却通过了", k)
		}
	}
}

func TestNewClientKeyRandomAndCustom(t *testing.T) {
	a := NewClientKey("x")
	if len(a.Key) != 3+32 || a.Key[:3] != "sk-" {
		t.Fatalf("随机密钥形态不对: %q", a.Key)
	}
	b, err := NewClientKeyWith("sk-meoo2api", "sk-meoo2api")
	if err != nil || b.Key != "sk-meoo2api" {
		t.Fatalf("自定义密钥未生效: %v %q", err, b.Key)
	}
	if _, err := NewClientKeyWith("x", "bad"); err == nil {
		t.Fatal("非法自定义值应报错")
	}
}
