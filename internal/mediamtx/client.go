package mediamtx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client 访问 MediaMTX 的管理 API,用来查询观众数。
type Client struct {
	base string
	http *http.Client
}

func NewClient(apiPort int) *Client {
	return &Client{
		base: fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		http: &http.Client{Timeout: 3 * time.Second},
	}
}

// 只取我们关心的字段
type pathsListResponse struct {
	ItemCount int `json:"itemCount"`
	Items     []struct {
		Name    string            `json:"name"`
		Ready   bool              `json:"ready"`
		Source  json.RawMessage   `json:"source"`
		Tracks  []string          `json:"tracks"`
		Readers []json.RawMessage `json:"readers"`
	} `json:"items"`
}

// PathState 是某条流路径的当前状态。
type PathState struct {
	Exists     bool
	Ready      bool   // 已有推流且可用
	HasSource  bool   // 有推流端连着
	Tracks     []string
	Viewers    int
}

// PathState 查询指定路径的状态。
//
// 返回的 error 表示"查询失败"(通常是 MediaMTX 还没起来或已退出);
// 调用方应据此区分"确实没人看"和"不知道有没有人看",不要把后者当成 0。
func (c *Client) PathState(path string) (*PathState, error) {
	resp, err := c.http.Get(c.base + "/v3/paths/list")
	if err != nil {
		return nil, fmt.Errorf("查询 MediaMTX API 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MediaMTX API 返回 %s", resp.Status)
	}

	var list pathsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("解析 MediaMTX API 响应失败: %w", err)
	}

	for _, it := range list.Items {
		if it.Name != path {
			continue
		}
		return &PathState{
			Exists:    true,
			Ready:     it.Ready,
			HasSource: len(it.Source) > 0 && string(it.Source) != "null",
			Tracks:    it.Tracks,
			Viewers:   len(it.Readers),
		}, nil
	}
	return &PathState{Exists: false}, nil
}
