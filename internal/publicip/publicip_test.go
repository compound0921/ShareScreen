package publicip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeRT 顶替真实网络。每个用例只声明"哪个 URL 回什么",不碰网。
type fakeRT struct {
	body   map[string]string // URL → 响应体
	status map[string]int    // URL → HTTP 状态码,缺省 200
	err    map[string]error  // URL → 传输层错误

	calls []string // 实际请求过的 URL,按顺序 —— 用来断言"没发多余请求"
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	f.calls = append(f.calls, u)

	if err := f.err[u]; err != nil {
		return nil, err
	}
	code := f.status[u]
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(f.body[u])),
		Header:     make(http.Header),
	}, nil
}

func newFake(rt *fakeRT) *Client {
	c := New(func(s string) bool { return isPublicIPv4ish(s) })
	c.Services = []string{"https://one.test", "https://two.test"}
	c.HTTP = &http.Client{Transport: rt}
	return c
}

// isPublicIPv4ish 是给测试用的极简校验:长得像纯 IPv4 且不是私网。
// 生产用的是 portmap.ExternalIPUsable,这里不把它搬过来 ——
// 那个函数有自己的测试。
func isPublicIPv4ish(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	first := parts[0]
	return first != "10" && first != "127" && first != "192"
}

func TestLookupFirstServiceWins(t *testing.T) {
	rt := &fakeRT{body: map[string]string{
		"https://one.test": "112.254.141.83\n",
		"https://two.test": "1.2.3.4",
	}}
	got, err := newFake(rt).Lookup(context.Background())

	if err != nil {
		t.Fatalf("Lookup 报错:%v", err)
	}
	if got.IP != "112.254.141.83" {
		t.Errorf("IP = %q,想要去掉换行后的 112.254.141.83", got.IP)
	}
	if got.Service != "https://one.test" {
		t.Errorf("Service = %q", got.Service)
	}
	// 第一个就成功,不该再去问第二个
	if len(rt.calls) != 1 {
		t.Errorf("应当只请求一次,实际 %v", rt.calls)
	}
}

// 第一个服务不通就试下一个 —— 这正是实测那个"ipify 连不上"的场景。
func TestLookupFallsThroughOnError(t *testing.T) {
	rt := &fakeRT{
		err:  map[string]error{"https://one.test": errors.New("connection refused")},
		body: map[string]string{"https://two.test": "112.254.141.83"},
	}
	got, err := newFake(rt).Lookup(context.Background())

	if err != nil {
		t.Fatalf("第二个服务能通,不该报错:%v", err)
	}
	if got.Service != "https://two.test" {
		t.Errorf("Service = %q,想要 https://two.test", got.Service)
	}
}

// 服务通了但回的是 IPv6 或一段文案 —— 跳过,不能拿去用。
// 实测 ifconfig.me 就会回 IPv6。
func TestLookupSkipsUnusableBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"IPv6", "2408:8214:b21:3a80::1"},
		{"中文文案", "当前 IP:2408:8214::1 来自于:中国 山东 青岛"},
		{"私网", "192.168.3.236"},
		{"空", ""},
		{"HTML", "<html><body>error</body></html>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := &fakeRT{body: map[string]string{"https://one.test": c.body}}
			_, err := newFake(rt).Lookup(context.Background())

			if err == nil {
				t.Fatal("不可用的返回值应当被跳过,最终报错")
			}
			// 两个服务都试过才算"跳过",不能第一个就放弃
			if len(rt.calls) != 2 {
				t.Errorf("应当把两个服务都试过,实际 %v", rt.calls)
			}
		})
	}
}

// 500 的响应体哪怕长得像个地址也不能要 —— 错误页里出现一个 IP 是常事。
func TestLookupSkipsNon200(t *testing.T) {
	rt := &fakeRT{
		status: map[string]int{"https://one.test": 500},
		body: map[string]string{
			// 两个服务都回同一个地址,区别只在前者状态码是 500
			"https://one.test": "112.254.141.83",
			"https://two.test": "112.254.141.83",
		},
	}
	got, err := newFake(rt).Lookup(context.Background())

	if err != nil {
		t.Fatalf("第二个服务是 200,不该报错:%v", err)
	}
	if got.Service != "https://two.test" {
		t.Errorf("Service = %q —— 第一个服务是 500,不该被采用", got.Service)
	}
}

// 全部失败返回错误(调用方据此不提供候选),而且不能 panic。
func TestLookupAllFail(t *testing.T) {
	rt := &fakeRT{err: map[string]error{
		"https://one.test": fmt.Errorf("超时"),
		"https://two.test": fmt.Errorf("超时"),
	}}
	if _, err := newFake(rt).Lookup(context.Background()); err == nil {
		t.Error("全部失败应当返回错误")
	}
}

// 默认服务清单不是空的,而且第一条是实测能通的那个。
//
// 这条挡的是"顺手把 ipify 换回第一位"之类的改动 —— 它在本机所在网络
// 根本连不上,而那是"教科书式首选",很容易被写回去。
func TestDefaultServicesStartWithMeasured(t *testing.T) {
	if len(Services) == 0 {
		t.Fatal("默认服务清单是空的")
	}
	if Services[0] != "https://ip.3322.net" {
		t.Errorf("第一条 = %q,应当是实测能通的那个", Services[0])
	}
}
