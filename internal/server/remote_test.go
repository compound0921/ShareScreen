package server

import (
	"errors"
	"testing"
)

// approveOpen 的三种落地结果。
//
// 这条路上最容易出问题的是**部分成功** —— 远控开不起来但共享能开。那种
// 情况下观众应该还能看画面,所以共享那一支必须照样走完;而"本来就在共享"
// 时又不能再 Start 一次(那会重启采集,观众那边是一次没必要的黑屏)。
func TestApproveOpenPartialSuccess(t *testing.T) {
	cases := []struct {
		name       string
		sharing    bool
		enableErr  error
		wantRemote bool
		wantStart  bool
		wantErr    bool
	}{
		{
			name:       "都成:远控开起来,共享从没在跑到开始跑",
			sharing:    false,
			wantRemote: true,
			wantStart:  true,
		},
		{
			name:       "本来就在共享:不该再 Start 一次",
			sharing:    true,
			wantRemote: true,
			wantStart:  false,
		},
		{
			name:      "远控开不起来:照样开始共享,能看不能控",
			enableErr: errors.New("当前采集源不支持远程控制(只有整屏采集能用)"),
			wantStart: true,
			wantErr:   true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			started := false
			res := approveOpen(
				c.sharing,
				func() { started = true },
				func(bool) error { return c.enableErr },
			)

			if started != c.wantStart {
				t.Errorf("start 被调用 = %v,想要 %v", started, c.wantStart)
			}
			if res.Remote != c.wantRemote {
				t.Errorf("Remote = %v,想要 %v", res.Remote, c.wantRemote)
			}
			if (res.Err != nil) != c.wantErr {
				t.Errorf("Err = %v,想要出错 = %v", res.Err, c.wantErr)
			}
			// 共享这一格永远是 true:要么本来就在跑,要么刚起来。远端要靠
			// 它把"能看不能控"和"整个没成"分开 —— 前者不必灰心。
			if !res.Sharing {
				t.Error("Sharing 应当为真:远控失败的那一支也得开始共享")
			}
		})
	}
}

// 开启动作必须带 true 下去。
//
// 这个函数的全部意义就是"把远控打开",传反了是灾难性的 —— 主机点了允许,
// 结果已经把远控关掉了,而观众那边显示"已开启"。同理,它也不能被调用两次
// (那会连带重启采集),所以这里顺带钉住调用次数。
func TestApproveOpenTurnsRemoteOn(t *testing.T) {
	var got []bool
	approveOpen(false, func() {}, func(on bool) error {
		got = append(got, on)
		return nil
	})

	if len(got) != 1 || !got[0] {
		t.Fatalf("enable 应当被调用一次且传 true,得到 %v", got)
	}
}
