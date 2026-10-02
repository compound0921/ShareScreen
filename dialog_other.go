//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// showFatalDialog 在非 Windows 平台上什么也不做 —— stderr 上已经能看到了。
func showFatalDialog(title, msg string) {}

// ask 在非 Windows 平台上走标准输入。
func ask(title, text string) bool {
	fmt.Printf("\n%s\n%s [y/N] ", title, text)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
