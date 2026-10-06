package notify

import (
	"strings"
	"testing"
	"time"
)

// 窗口那一半在真机上很难断言,而措辞是用户唯一读到的东西 —— 所以把它
// 单独拆成纯函数测。
func TestDescribe(t *testing.T) {
	tests := []struct {
		name       string
		req        Request
		wantInBody []string
	}{
		{
			name:       "普通申请",
			req:        Request{IP: "192.168.3.163", Timeout: 60 * time.Second},
			wantInBody: []string{"192.168.3.163"},
		},
		{
			name:       "拿不到对方地址时也要说清楚",
			req:        Request{IP: "", Timeout: 60 * time.Second},
			wantInBody: []string{"没有留下地址"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, body := Describe(tc.req)
			if title == "" {
				t.Error("标题不能为空")
			}
			for _, want := range tc.wantInBody {
				if !strings.Contains(body, want) {
					t.Errorf("正文里没有 %q:%q", want, body)
				}
			}
		})
	}
}

// 秒数**不能**被烤进正文。
//
// 倒计时每秒都在变,而 Describe 只在弹窗创建时跑一次 —— 秒数一旦写进
// 正文,显示的就永远是弹出那一刻的数字,之后一动不动。这条测的就是
// 那个回归:正文里不该出现任何秒数。
func TestDescribeHasNoBakedInSeconds(t *testing.T) {
	_, body := Describe(Request{IP: "10.0.0.1", Timeout: 47 * time.Second})
	if strings.Contains(body, "47") || strings.Contains(body, "秒") {
		t.Errorf("正文里烤进了秒数,倒计时就不会动了:%q", body)
	}
}

func TestCountdownText(t *testing.T) {
	tests := []struct {
		name      string
		remaining time.Duration
		wantIn    string
		wantOut   string
	}{
		{"整秒", 47 * time.Second, "47 秒后自动拒绝", ""},
		// 向上取整:剩 59.4 秒时说"60 秒"才是对的。截断的话弹窗一出来
		// 就显示 59,看着像已经过了一秒。
		{"不足一秒也要算一秒", 400 * time.Millisecond, "1 秒后自动拒绝", "0 秒"},
		{"向上取整到 60", 59*time.Second + 400*time.Millisecond, "60 秒后自动拒绝", "59 秒"},
		{"归零不再说秒数", 0, "即将自动拒绝", "0 秒"},
		{"负数同样处理", -5 * time.Second, "即将自动拒绝", "0 秒"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CountdownText(tc.remaining)
			if !strings.Contains(got, tc.wantIn) {
				t.Errorf("CountdownText(%v) = %q,应当包含 %q", tc.remaining, got, tc.wantIn)
			}
			if tc.wantOut != "" && strings.Contains(got, tc.wantOut) {
				t.Errorf("CountdownText(%v) = %q,不该包含 %q", tc.remaining, got, tc.wantOut)
			}
		})
	}
}

// 文案把超时说成"自动拒绝",这是**用户可见的结果**,不是弹窗的动作。
//
// 这条钉住的是那个区分不要被改坏:超时之后申请在控制权状态机里确实是
// denied,所以对用户说"自动拒绝"没有错;但弹窗自己**不发送任何决定**,
// 作废仍由 remotectl 的过期循环做。哪天有人看到这句文案、顺手在
// popup.go 里补一个 Deny 调用,就重复了,而观众收到的原因也会从
// "主机没有应答"变成"主机拒绝了这次申请" —— 那是另一回事。
func TestCountdownCopyMatchesMechanism(t *testing.T) {
	got := CountdownText(30 * time.Second)
	if !strings.Contains(got, "自动拒绝") {
		t.Errorf("文案应当是「N 秒后自动拒绝」:%q", got)
	}
	if strings.Contains(got, "还剩") {
		t.Errorf("文案里不该再有「还剩」:%q", got)
	}
}
