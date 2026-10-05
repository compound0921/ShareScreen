package remotectl

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// tokenBytes 是链接令牌的熵。32 字节 = 256 位。
//
// 这么长的直接后果是:不需要对 /ws 做失败次数限流。猜中一个 256 位随机
// 值的概率低到不值得为它写代码。
const tokenBytes = 32

// NewToken 生成一个新的链接令牌。
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// URL 安全的 base64 且不带填充:令牌要出现在链接里,而 # 和 =
	// 在复制粘贴、二维码里都是麻烦。
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenMatch 比较两个令牌。
//
// 用 subtle.ConstantTimeCompare 而不是 ==:== 在第一个不同的字节上就
// 返回了,比较耗时会随匹配前缀长度变化。这个项目是个人自用,威胁模型
// 够不上计时攻击,但恒定时间比较也就一行,没有理由不写对。
func tokenMatch(want, got string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// tokenFromRequest 从请求里取出令牌。
//
// 只看查询参数。观看页把令牌放在 URL 的 # 片段里,由页面脚本取出来
// 拼到 WebSocket 的地址上 —— 片段不会被发到服务器,所以页面本身的
// 请求在日志里是不带凭据的。
func tokenFromRequest(r *http.Request) string {
	return r.URL.Query().Get("t")
}
