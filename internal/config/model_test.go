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

// LanHost 会被拼进 "http://<这里>:8889/...",所以任何会让它拼出死链的输入
// 都要挡掉。注意**不要求**它是个能解析的 IP —— 主机名要留得住。
func TestNormalizeLanHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通 IP", "192.168.3.236", "192.168.3.236"},
		{"主机名要留得住", "myhost.local", "myhost.local"},
		{"首尾空白", "  192.168.3.236  ", "192.168.3.236"},
		{"粘进来一个完整 URL —— 剥掉协议头", "https://192.168.3.236", "192.168.3.236"},
		{"http 前缀同样剥掉", "http://myhost.local", "myhost.local"},
		// 下面这些拼出来是死链,整条丢弃、退回自动
		{"带端口 —— 端口由程序补,不该写在这里", "192.168.3.236:8889", ""},
		{"带路径", "192.168.3.236/live", ""},
		{"中间有空格", "192.168 3.236", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{LanHost: c.in}
			cfg.Normalize()
			if cfg.LanHost != c.want {
				t.Errorf("Normalize(%q) → %q,想要 %q", c.in, cfg.LanHost, c.want)
			}
		})
	}
}

// LanHost 不该被动过 PublicHostAuto 那套机制 —— 它没有自动写入者,
// 所以也没有 auto 标记可言。
func TestLanHostDoesNotTouchPublicHostAuto(t *testing.T) {
	cfg := Config{PublicHost: "1.2.3.4", PublicHostAuto: true, LanHost: "192.168.1.5"}
	cfg.Normalize()

	if !cfg.PublicHostAuto {
		t.Error("LanHost 的存在不该影响 PublicHostAuto")
	}
}

// 老配置文件里没有这个字段,读进来必须是"自动"。
func TestDefaultHasNoLanHost(t *testing.T) {
	if got := Default().LanHost; got != "" {
		t.Errorf("默认 LanHost = %q,想要空(空=自动)", got)
	}
}
