package model

import (
	"encoding/json"
	"testing"
)

// 老配置文件里没有 early_header_sec：必须补默认值，否则升级后新行为永远不生效。
func TestConfigUnmarshalFillsEarlyHeaderDefault(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"keepalive_sec":12,"request_timeout_sec":180}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.EarlyHeaderSec != DefaultConfig().EarlyHeaderSec {
		t.Fatalf("缺字段应补默认值 %d，得到 %d", DefaultConfig().EarlyHeaderSec, c.EarlyHeaderSec)
	}
}

// 显式写 0 是「关闭」，不能被默认值覆盖——这是运维回滚新行为的唯一开关。
func TestConfigUnmarshalHonorsExplicitZero(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"early_header_sec":0}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.EarlyHeaderSec != 0 {
		t.Fatalf("显式 0 必须保留，得到 %d", c.EarlyHeaderSec)
	}
	var c2 Config
	if err := json.Unmarshal([]byte(`{"early_header_sec":15}`), &c2); err != nil {
		t.Fatal(err)
	}
	if c2.EarlyHeaderSec != 15 {
		t.Fatalf("显式值必须保留，得到 %d", c2.EarlyHeaderSec)
	}
}

// 序列化要能带上新字段，否则面板保存一次就把它丢了。
func TestConfigMarshalKeepsEarlyHeader(t *testing.T) {
	b, err := json.Marshal(Config{EarlyHeaderSec: 9})
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.EarlyHeaderSec != 9 {
		t.Fatalf("往返后应保持 9，得到 %d", back.EarlyHeaderSec)
	}
}
