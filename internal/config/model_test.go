package config

import "testing"

// 自动端口映射能不能改写公网地址 —— 这条规则决定了用户手填的 DDNS 域名
// 会不会被程序悄悄覆盖掉。
func TestCanAutoSetPublicHost(t *testing.T) {
	cases := []struct {
		name string
		c    Config
		want bool
	}{
		{"空地址", Config{}, true},
		{"上次是自动写的", Config{PublicHost: "1.2.3.4:8889", PublicHostAuto: true}, true},
		{"用户手填的域名", Config{PublicHost: "share.example.com:8443"}, false},
		{"用户手填的 IP", Config{PublicHost: "1.2.3.4:8889"}, false},
	}
	for _, c := range cases {
		if got := c.c.CanAutoSetPublicHost(); got != c.want {
			t.Errorf("%s: CanAutoSetPublicHost() = %v,想要 %v", c.name, got, c.want)
		}
	}
}

// 地址被清空之后,"自动写入"这个标记就没有意义了,必须一起归零 ——
// 否则配置里会出现"没有公网地址,但它是自动写的"这种自相矛盾的状态。
func TestNormalizeClearsAutoFlagWhenHostEmpty(t *testing.T) {
	c := Config{PublicHostAuto: true}
	c.Normalize()

	if c.PublicHostAuto {
		t.Error("公网地址为空时 PublicHostAuto 应当被归零")
	}
}

func TestNormalizeKeepsAutoFlagWithHost(t *testing.T) {
	c := Config{PublicHost: "1.2.3.4:8889", PublicHostAuto: true}
	c.Normalize()

	if !c.PublicHostAuto {
		t.Error("有公网地址时不该被动过")
	}
}

// 老配置文件里没有这两个字段,读进来必须是"关闭"而不是别的什么。
// 这是新功能默认关闭的保证。
func TestDefaultHasAutoPortMapOff(t *testing.T) {
	d := Default()
	if d.AutoPortMap {
		t.Error("自动端口映射必须默认关闭")
	}
	if d.PublicHostAuto {
		t.Error("默认配置不该声称公网地址是自动写入的")
	}
}
