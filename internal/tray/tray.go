// Package tray 提供系统托盘图标。
//
// 有了它,程序可以不带控制台窗口在后台常驻 —— 用图标表示"我在跑",
// 菜单项提供快捷操作,双击图标打开控制页。
package tray

import (
	"time"

	"github.com/energye/systray"
)

// Actions 是托盘菜单触发的动作。任一字段为 nil 表示该项不生效。
type Actions struct {
	OpenControl func() // 打开控制页
	StartShare  func()
	StopShare   func()
	Quit        func() // 退出前的收尾,之后托盘自行结束

	// RemoteControlOn 报告远控当前是否开着。
	//
	// 菜单的勾选态以它为准,而不是"上次点的时候是不是成功":控制页那边
	// 也能开关,而在托盘的闭包里记状态会和它走散。
	RemoteControlOn func() bool

	// ToggleRemoteControl 开/关远控,返回**实际**结果。
	//
	// 返回真实状态而不是让调用方假设成功:开启会失败(端口被占、采集源
	// 不支持),而勾必须反映实际状态 —— 勾着一个其实没开的开关,是托盘
	// 菜单里最容易误导人的一种错。
	ToggleRemoteControl func(on bool) bool

	// RevokeControl 立即收回控制权。
	//
	// 这是不依赖浏览器页面的那个刹车:页面卡住、断网、打不开的时候,
	// 主人仍然要能把正在操作自己电脑的人踢开。
	RevokeControl func()

	// OnReady 在托盘就绪之后调用一次。
	//
	// 拿它来决定什么时候可以安全地调 SetStatus:在那之前 systray 会把
	// 每一次调用都丢掉,并且往日志里写一行 "tray not ready yet"。
	// 那是调用方自己制造的噪音,而且会淹没真正的错误。
	OnReady func()
}

// 菜单刷新间隔。
//
// 远控状态可能被控制页那边改掉,而托盘菜单只在 onReady 里建一次。
// 没有回调可挂,就用轮询 —— 两秒一次,代价可以忽略。
const menuSyncInterval = 2 * time.Second

// Run 建立托盘图标并进入消息循环。
//
// 会阻塞,应在独立的 goroutine 里调用。
func Run(a Actions) {
	// 所有设置都必须在 onReady 回调里做。
	// 在 systray.Run 之前调用 SetIcon / SetTooltip 会报
	// "tray not ready yet" 并被静默丢弃 —— 图标不会出现。
	systray.Run(func() {
		if data := trayIcon(); data != nil {
			systray.SetIcon(data)
		}
		systray.SetTooltip("ShareScreen")

		// 双击图标 = 打开控制页。这是最常用的动作,给最快的路径。
		systray.SetOnDClick(func(systray.IMenu) {
			if a.OpenControl != nil {
				a.OpenControl()
			}
		})

		mOpen := systray.AddMenuItem("打开控制页", "在浏览器中打开控制页")
		systray.AddSeparator()
		mStart := systray.AddMenuItem("开始共享", "开始推流")
		mStop := systray.AddMenuItem("停止共享", "停止推流")
		systray.AddSeparator()

		if a.RemoteControlOn != nil {
			addRemoteControlMenu(a)
			systray.AddSeparator()
		}

		mQuit := systray.AddMenuItem("退出", "停止共享并退出程序")

		mOpen.Click(func() {
			if a.OpenControl != nil {
				a.OpenControl()
			}
		})
		mStart.Click(func() {
			if a.StartShare != nil {
				a.StartShare()
			}
		})
		mStop.Click(func() {
			if a.StopShare != nil {
				a.StopShare()
			}
		})
		mQuit.Click(func() {
			if a.Quit != nil {
				a.Quit()
			}
			systray.Quit()
		})

		// 菜单建完了,这时候 SetStatus 才不会白调。
		if a.OnReady != nil {
			a.OnReady()
		}
	}, func() {})
}

// addRemoteControlMenu 加上远程控制这一组菜单项。
//
// 必须在 onReady 里调用 —— 菜单只能在托盘就绪之后建。
func addRemoteControlMenu(a Actions) {
	mAllow := systray.AddMenuItemCheckbox(
		"允许远程控制",
		"开启后观众可在你批准之后操作这台电脑",
		a.RemoteControlOn())

	mRevoke := systray.AddMenuItem("断开远程控制", "立即收回控制权")

	mAllow.Click(func() {
		// 点击的目标状态就是当前勾选态的反面 —— systray 不会自己翻转,
		// 勾选态完全由这里控制。
		want := !mAllow.Checked()

		ok := want
		if a.ToggleRemoteControl != nil {
			ok = a.ToggleRemoteControl(want)
		}
		// 勾必须反映**实际**状态。开启可能失败(端口被占、当前采集源
		// 不支持),这时把勾打上就是在骗人。
		if ok {
			mAllow.Check()
		} else {
			mAllow.Uncheck()
		}
	})

	mRevoke.Click(func() {
		if a.RevokeControl != nil {
			a.RevokeControl()
		}
	})

	// 控制页那边也能开关,而菜单只在上面建这一次 —— 靠轮询把勾选态
	// 拉回来。没有更省的办法:systray 没有"状态变了通知我"这种回调。
	go func() {
		t := time.NewTicker(menuSyncInterval)
		defer t.Stop()
		for range t.C {
			on := a.RemoteControlOn()
			if on != mAllow.Checked() {
				if on {
					mAllow.Check()
				} else {
					mAllow.Uncheck()
				}
			}
		}
	}()
}

// SetStatus 更新鼠标悬停时显示的提示文字。
func SetStatus(text string) {
	systray.SetTooltip("ShareScreen — " + text)
}
