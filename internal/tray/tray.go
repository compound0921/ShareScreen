// Package tray 提供系统托盘图标。
//
// 有了它,程序可以不带控制台窗口在后台常驻 —— 用图标表示"我在跑",
// 菜单项提供快捷操作,双击图标打开控制页。
package tray

import "github.com/energye/systray"

// Actions 是托盘菜单触发的动作。任一字段为 nil 表示该项不生效。
type Actions struct {
	OpenControl func() // 打开控制页
	StartShare  func()
	StopShare   func()
	Quit        func() // 退出前的收尾,之后托盘自行结束
}

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
	}, func() {})
}

// SetStatus 更新鼠标悬停时显示的提示文字。
func SetStatus(text string) {
	systray.SetTooltip("ShareScreen — " + text)
}
