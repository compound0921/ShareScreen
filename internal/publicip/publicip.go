// Package publicip 问一个外部回显服务:本机的公网出口地址是什么。
//
// 这是本程序**唯一**对外的网络请求,而且只在路由器(UPnP)拿不到外网地址时
// 才发 —— UPnP 有结果就一个包都不发。
//
// # 它给出的地址不代表端口映射能用
//
// 运营商级 NAT 下,回显服务照样返回一个漂亮的公网地址,但那是运营商那台 NAT
// 的出口,不是能映射进来的入口。**没有任何办法从外面判断这件事** ——
// 只有路由器自己知道它有没有把端口映射出去。
//
// 所以这个值只能当参考,绝不能拿去自动填写公网地址:那会悄悄产出一条打不开
// 的链接,比留空更糟(留空至少用户在界面上看得见"没填")。
package publicip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// requestTimeout 是单次请求的上限。
	//
	// 必须显式设:零值 http.Client 等于永不超时,而这是唯一一个会把我们
	// 挂在外部服务上的地方(同类教训见 §7.6 里 goupnp 那条)。
	requestTimeout = 3 * time.Second

	// maxBody 是响应体的读取上限。回显服务只该回一行 IP,1KB 足够 ——
	// 也免得一个坏掉的服务把内存吃掉。
	maxBody = 1024
)

// Services 是按顺序尝试的回显服务,**按实测结果排的**,不是照抄"常见服务"。
//
// 2026-10-07 在本机所在网络实测:
//
//	✓ https://ip.3322.net         纯 IPv4,0.45s
//	✓ https://ipv4.icanhazip.com  纯 IPv4,1.25s
//	✗ https://4.ipw.cn            连不上
//	✗ https://api.ipify.org       连不上 —— 那种"教科书式首选"在这里根本不通
//	  https://ifconfig.me/ip      返回 IPv6,会被校验挡掉
//	  https://myip.ipip.net       返回 IPv6 + 中文文本,要解析才能用
//
// 所以"按顺序试几个"是必需的,不是保险:单独押一个服务,换一个网络环境就废了。
//
// 只收**纯文本返回 IPv4** 的服务。"当前 IP:… 来自于…"那种要写解析器,而人家
// 的文案并不承诺不变,不值得为一个可选增强养一个解析器。
var Services = []string{
	"https://ip.3322.net",
	"https://ipv4.icanhazip.com",
}

// Result 是一次成功的探测。
type Result struct {
	IP      string // 探测到的地址
	Service string // 哪个服务给的,排障时有用
}

// Client 依次问一组回显服务。
//
// 零值不可用,用 New 构造。
type Client struct {
	HTTP *http.Client

	// Services 为空时用包级的 Services。
	Services []string

	// Validate 判断一个返回值能不能用;返回 false 就试下一个服务。
	//
	// 由调用方注入(生产里是 portmap.ExternalIPUsable),这样这个包不必
	// 知道"什么算公网地址"—— 那套判断已经有主了,抄一份迟早会走散。
	Validate func(string) bool
}

// New 构造一个客户端。validate 传 nil 表示不做校验(不建议)。
func New(validate func(string) bool) *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: requestTimeout},
		Validate: validate,
	}
}

// Lookup 依次问各个服务,返回第一个通过校验的结果。
//
// 全部失败会返回错误,但**调用方不该把它当故障报给用户**:这只是个可选增强,
// 拿不到就不提供那个候选,页面照常。
func (c *Client) Lookup(ctx context.Context) (Result, error) {
	services := c.Services
	if len(services) == 0 {
		services = Services
	}

	var lastErr error
	for _, svc := range services {
		ip, err := c.ask(ctx, svc)
		if err != nil {
			lastErr = err
			continue
		}
		if c.Validate != nil && !c.Validate(ip) {
			// 服务通了但回的不是能用的地址(IPv6、私网、或者一段文案)。
			// 换下一个 —— 这正是 ifconfig.me 那类服务会被跳过的地方。
			lastErr = fmt.Errorf("%s 返回的 %q 不能用作公网 IPv4", svc, ip)
			continue
		}
		return Result{IP: ip, Service: svc}, nil
	}

	if lastErr == nil {
		lastErr = errors.New("没有配置回显服务")
	}
	return Result{}, lastErr
}

// ask 请求一个服务,返回去掉首尾空白的响应体。
func (c *Client) ask(ctx context.Context, svc string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s 返回 HTTP %d", svc, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
