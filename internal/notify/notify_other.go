//go:build !windows

package notify

import "context"

// AskButtons 在非 Windows 上不弹窗,直接返回"没有做决定"。
//
// 本程序是 Windows 专用的,这个存根只是为了让 `GOOS=linux go build ./...`
// 之类的交叉检查能过。返回 −1 而不是报错:调用方对"没有决定"的处理本来
// 就是什么都不做,这正是这里想要的行为。
func AskButtons(context.Context, Prompt) int { return -1 }
