package config

import (
	"strings"
	"testing"
)

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

// 控制密码的首尾空白必须去掉:粘贴时多带一个空格在界面上看不出来,
// 而填密码的人永远对不上 —— 表现为"密码明明是对的,就是不通过"。
func TestNormalizeControlPassword(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通密码原样保留", "hunter2", "hunter2"},
		{"内部空格是密码的一部分", "hunter 2", "hunter 2"},
		{"首尾空白去掉", "  hunter2  ", "hunter2"},
		{"全是空白 = 没设", "   ", ""},
		{"空就是空", "", ""},
		{"长得离谱 —— 当成文件写坏了清掉", strings.Repeat("x", 300), ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{}
			cfg.RemoteControl.ControlPassword = c.in
			cfg.Normalize()
			if got := cfg.RemoteControl.ControlPassword; got != c.want {
				t.Errorf("Normalize(%q) → %q,想要 %q", c.in, got, c.want)
			}
		})
	}
}

// 控制密码的默认值是 123456,不是空。
//
// 这里和**令牌**的规则是相反的(令牌默认不生成,免得配置文件里躺着一串
// 没人认识的秘密),因为要的东西正相反:令牌是这台机器独有的秘密,而
// 控制密码图的是"对方直接填就知道",不用主机先设一次再告诉人。
func TestDefaultHasControlPassword(t *testing.T) {
	if got := Default().RemoteControl.ControlPassword; got != "123456" {
		t.Errorf("默认 ControlPassword = %q,想要 123456", got)
	}
}

// 但清空必须仍然是有效的设定。
//
// Normalize 不管密码这一格 —— 一旦它把空值填回默认,用户"清空 = 关掉这条
// 捷径"就会在下次启动时自己长回来,而且没有任何提示。
func TestNormalizeKeepsClearedPassword(t *testing.T) {
	cfg := Config{}
	cfg.RemoteControl.ControlPassword = ""
	cfg.Normalize()
	if got := cfg.RemoteControl.ControlPassword; got != "" {
		t.Errorf("清空之后 Normalize 不该填回来,得到 %q", got)
	}
}

// 请求入口默认开 —— 这和项目里别的对外开关是反的。
//
// 别的对外能力(远控、AutoPortMap)默认关,理由是"对外动作不该在用户没
// 要求的时候做"。这一条是唯一的例外:默认开,换来"把链接发出去,对方
// 点一下就能请求"。
//
// 这个测试把这个取舍钉住:谁顺手把它改成默认关,会在这里读到为什么不能,
// 而不是只看到一行测试变红。
func TestDefaultHasRequestEntry(t *testing.T) {
	if !Default().RemoteControl.RequestEntry {
		t.Error("请求入口应当默认打开:关掉它,远控没开时对方就够不着主机," +
			"整条「请求开启」链路等于不存在")
	}
}

// MappedPort 跟着"端口会不会在监听"走,不跟着"远控开没开"走。
//
// 第二条用例是这里的关键:只有请求入口开着的时候也必须映射 —— 请求入口
// 存在的唯一意义就是远端够得到它,不映射就只有局域网内能用,名不副实。
func TestMappedPortFollowsListening(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		entry   bool
		want    int
	}{
		{"两个都不开:一条映射都不留", false, false, 0},
		{"只有请求入口:要映射", false, true, 8090},
		{"只有远控:要映射", true, false, 8090},
		{"两个都开:要映射", true, true, 8090},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := RemoteControlConfig{Port: 8090, Enabled: c.enabled, RequestEntry: c.entry}
			if got := r.MappedPort(); got != c.want {
				t.Errorf("Enabled=%v RequestEntry=%v → %d,想要 %d",
					c.enabled, c.entry, got, c.want)
			}
		})
	}
}

// WGC 是新增的采集源:Normalize 不能把它当成未知值回退掉,默认源也不能
// 因此改变 —— 第一阶段它是手动选项。
func TestWGCSourceSurvivesNormalize(t *testing.T) {
	if !ValidSource(SourceScreenWGC) {
		t.Fatal("SourceScreenWGC 应当是合法采集源")
	}

	cfg := Config{Video: VideoConfig{Source: SourceScreenWGC}}
	cfg.Normalize()
	if cfg.Video.Source != SourceScreenWGC {
		t.Errorf("Normalize 把 WGC 源改成了 %q", cfg.Video.Source)
	}

	if got := Default().Video.Source; got != SourceScreenWGC {
		t.Errorf("默认采集源 = %q,想要 %q", got, SourceScreenWGC)
	}
}

// 帧率只保留 30 和 60;旧配置里的 120/20/15 在 Normalize 时回到默认 60。
func TestNormalizeFPS(t *testing.T) {
	for _, fps := range []int{0, -1, 15, 20, 24, 120, 144, 240} {
		cfg := Config{Video: VideoConfig{FPS: fps}}
		cfg.Normalize()
		if got := cfg.Video.FPS; got != 60 {
			t.Errorf("FPS=%d 归一化后 = %d,想要 60", fps, got)
		}
	}
	for _, fps := range []int{30, 60} {
		cfg := Config{Video: VideoConfig{FPS: fps}}
		cfg.Normalize()
		if got := cfg.Video.FPS; got != fps {
			t.Errorf("FPS=%d 不该被改,得到 %d", fps, got)
		}
	}
}
