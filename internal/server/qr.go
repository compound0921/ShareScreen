package server

import (
	"net/http"

	qrcode "github.com/skip2/go-qrcode"

	"sharescreen/internal/config"
)

// handleQR 生成观看地址的二维码 PNG。
//
// 二维码在服务端生成而不是用前端 JS 库,是为了让控制页保持零第三方依赖 ——
// 页面要能在无外网的局域网环境里正常工作。
//
// 默认用首选观看地址(有公网地址就用公网的,手机扫码走公网);
// 也可以用 ?t= 显式指定内容。
func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("t")
	if text == "" {
		text = s.preferredWatchURL(s.currentConfig())
	}
	if text == "" {
		http.Error(w, "没有可用的观看地址", http.StatusNotFound)
		return
	}

	png, err := qrcode.Encode(text, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "生成二维码失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// preferredWatchURL 返回最适合给手机扫的地址:优先公网,其次局域网。
func (s *Server) preferredWatchURL(cfg config.Config) string {
	urls := s.watchURLs(cfg)
	for _, u := range urls {
		if u.Kind == "public" {
			return u.URL
		}
	}
	if len(urls) > 0 {
		return urls[0].URL
	}
	return ""
}
