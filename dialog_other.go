//go:build !windows

package main

// showFatalDialog 在非 Windows 平台上什么也不做 —— stderr 上已经能看到了。
func showFatalDialog(title, msg string) {}
