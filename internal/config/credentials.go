package config

import (
	"crypto/rand"
	"math/big"
)

// 去掉容易看错的 l / o / 0 / 1 —— 这个密码要被人手抄或从二维码里认,
// 少一个歧义字符就少一次"输错了但不知道错在哪"。
const passwordAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// GeneratePassword 生成一个随机密码。
func GeneratePassword(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[idx.Int64()]
	}
	return string(out), nil
}

// EnsureCredentials 确保配置里有观看凭据,缺失时补上。
// 返回是否发生了变更 —— 调用方据此决定要不要写回磁盘。
func (c *Config) EnsureCredentials() (bool, error) {
	changed := false

	if c.ViewerUser == "" {
		c.ViewerUser = "viewer"
		changed = true
	}
	if c.ViewerPass == "" {
		pw, err := GeneratePassword(12)
		if err != nil {
			return false, err
		}
		c.ViewerPass = pw
		changed = true
	}
	return changed, nil
}
