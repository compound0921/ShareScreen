//go:build !windows

package tray

// afterMenuClosed 在非 Windows 上是空操作。
//
// SetForegroundWindow + TrackPopupMenu + PostMessage(WM_NULL) 这套搭配是
// Win32 特有的,别的平台上没有对应的问题。
func afterMenuClosed() {}
