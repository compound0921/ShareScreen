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
// 开启请求的措辞必须把"同意之后会发生什么"说全。
//
// 这个功能把原来分开的两次确认(开共享 / 允许操作)合成了一次,所以这一句
// 话是主机唯一的知情来源。少了"会开始共享画面"这半句,主机无从知道自己的
// 屏幕将被共享出去 —— 他看到的只是"有人请求开启",而"开启"指什么并不显然。
func TestDescribeOpenRequest(t *testing.T) {
	title, body := Describe(Request{IP: "10.0.0.1", Timeout: time.Minute, Kind: KindOpen})

	if !strings.Contains(title, "共享") {
		t.Errorf("开启请求的标题要提到共享,得到 %q", title)
	}
	for _, want := range []string{"10.0.0.1", "共享", "操作"} {
		if !strings.Contains(body, want) {
			t.Errorf("正文里没有 %q:%q", want, body)
		}
	}

	// 控制申请的措辞**不能**被带上这句 —— 那种情况下主机根本没同意过
	// 共享画面,写上去是吓人。
	_, ctlBody := Describe(Request{IP: "10.0.0.1", Timeout: time.Minute})
	if strings.Contains(ctlBody, "共享") {
		t.Errorf("控制申请的正文不该提共享:%q", ctlBody)
	}
}

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

// 按钮下标到 Choice 的映射是"窗口泛化"和"批准语义"之间的接缝,单拎出来测。
//
// 这里最容易出的错是**把两个按钮弄反**:允许和拒绝反过来的后果不是弹窗
// 难看,是主人点「拒绝」把机器交了出去。
func TestChoiceForIndex(t *testing.T) {
	tests := []struct {
		name string
		idx  int
		want Choice
	}{
		{"下标 0 是右下角那个主色按钮,也就是允许", 0, ChoiceAllow},
		{"下标 1 是它左边那个,也就是拒绝", 1, ChoiceDeny},
		{"−1 表示没有做决定(取消、关窗、窗口建不起来)", -1, ChoiceNone},
		{"越界的下标不能悄悄变成某个按钮", 2, ChoiceNone},
		{"负数越界同样", -7, ChoiceNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := choiceForIndex(tc.idx); got != tc.want {
				t.Errorf("choiceForIndex(%d) = %v,想要 %v", tc.idx, got, tc.want)
			}
		})
	}
}
