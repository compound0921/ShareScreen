//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procMessageBoxW = user32.NewProc("MessageBoxW")
	// GetConsoleWindow 在 kernel32 里,不在 user32 —— 放错会 panic。
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

const (
	mbOK            = 0x00000000
	mbYesNo         = 0x00000004
	mbIconError     = 0x00000010
	mbIconQuestion  = 0x00000020
	mbSetForeground = 0x00010000
	mbTopmost       = 0x00040000
	// 把默认按钮挪到第二个。这一条是安全底线,不是审美选择 —— 见 ask 的说明。
	mbDefButton2 = 0x00000100

	idYes = 6 // MessageBox 点"是"时返回的值
)

// ask 提一个是/否问题,返回用户是否选了"是"。
//
// 有控制台时走标准输入,没有才弹框。这一点和 showFatalDialog 相反,是有意的:
// 从终端或脚本启动时,一个模态对话框会把自动化直接卡死在那儿等人点。
//
// 对话框的**默认按钮被刻意挪到了"否"**(MB_DEFBUTTON2),这是条安全底线。
// 实测:在没有人在场的环境里(自动化截屏、无人值守启动),对话框会被人或
// 系统按默认按钮关掉 —— 默认是"是"的时候,一个无人值守的启动会不问自答地
// 执行"结束进程""改端口"这类动作。默认设成"否",这类情况就一律落到安全的
// 一侧:想执行有风险的动作,必须真的有人去点它。
func ask(title, text string) bool {
	if hasConsole() {
		fmt.Printf("\n%s\n%s [y/N] ", title, text)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		return line == "y" || line == "yes"
	}

	// 报错路径上的代码不能再自己崩一次,这里同样先确认 API 可用。
	if procMessageBoxW.Find() != nil {
		return false
	}
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	m, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return false
	}
	r, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbYesNo|mbIconQuestion|mbSetForeground|mbTopmost|mbDefButton2)
	return int32(r) == idYes
}

// showFatalDialog 用模态对话框把启动失败的原因告诉用户。
//
// 只在进程没有控制台时弹。双击 exe 启动(构建时带 -H=windowsgui)正是这种
// 情况:错误只落在日志文件里,而没人会为了一次"闪退"去翻日志。从终端启动时
// stderr 已经能看见,再弹个模态框只会挡住脚本。
func showFatalDialog(title, msg string) {
	// 这条路径上不能再抛异常 —— 那会把真正要报的错盖掉。
	// 正常系统上这两个过程一定存在,但"报错时再崩一次"的代价太大。
	if procGetConsoleWindow.Find() != nil || procMessageBoxW.Find() != nil {
		return
	}
	if hasConsole() {
		return
	}
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	procMessageBoxW.Call(
		0, // 无父窗口
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbOK|mbIconError|mbSetForeground|mbTopmost)
}

// hasConsole 报告当前进程是否挂着控制台窗口。
func hasConsole() bool {
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}
