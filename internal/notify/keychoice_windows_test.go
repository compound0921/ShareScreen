//go:build windows

package notify

import "testing"

// 键盘路由是这个弹窗唯一的输入安全约束,所以单拎出来测。
func TestKeyChoice(t *testing.T) {
	tests := []struct {
		name    string
		message uint32
		vk      uintptr
		want    keyAction
	}{
		// 这条是整套设计的底线。窗口是抢焦点弹出来的,用户那个回车
		// 几乎肯定是打给别的窗口的 —— 它必须被吃掉,而且不能产生任何
		// 选择。注意"吃掉"和"不管"是两回事:不管的话回车会落到对话框
		// 管理器手里去激活默认按钮。
		{"Enter 只吃掉,不产生任何选择", wmKeyDown, vkReturn, keyConsume},
		{"Esc 落在左边那个按钮上 —— 方向安全,而且是唯一的取消方式", wmKeyDown, vkEscape, keySecondary},
		{"其他按键交回系统", wmKeyDown, 'A', keyIgnore},
		{"Tab 之类的也交回系统", wmKeyDown, 0x09, keyIgnore},

		// 只有 WM_KEYDOWN 算数 —— 抬起、字符、系统键都不能产生选择
		{"按键抬起不产生选择", 0x0101, vkReturn, keyIgnore},
		{"Alt 组合键不产生选择", 0x0104, 'Y', keyIgnore},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyChoice(tc.message, tc.vk); got != tc.want {
				t.Errorf("keyChoice(msg=%#x, vk=%#x) = %v,想要 %v",
					tc.message, tc.vk, got, tc.want)
			}
		})
	}
}

// 单独把这条钉死:Enter **不能产生任何选择**。
//
// 上面那张表已经覆盖了它,但它值得一条自己的用例 —— 它红掉的时候,
// 读测试的人应当立刻明白出了什么事,而不是从一张表里推论。
//
// 注意主按钮那一侧根本没有键盘路径(keyAction 里没有对应 keyPrimary 的
// 值):键盘只可能是"什么都不做"或"按下右边那个按钮",两个方向都是安全的 ——
// 允许/切换都只能靠鼠标点。
func TestEnterNeverDecides(t *testing.T) {
	for _, m := range []uint32{wmKeyDown, 0x0101, 0x0104, 0x0106} {
		if got := keyChoice(m, vkReturn); got == keySecondary {
			t.Errorf("msg=%#x 时回车按下了按钮 —— 它应当什么都不做", m)
		}
	}
}
