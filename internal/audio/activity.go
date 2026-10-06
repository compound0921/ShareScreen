// 这个文件里的东西刻意不带平台标记:静音计时和设备挑选都是纯逻辑,
// 在任何平台上都能编译、能测。真正碰 COM 的那一半在 capture_windows.go。
package audio

import (
	"sync/atomic"
	"time"
)

// audibility 记着"上一次真正听到声音是什么时候"。
//
// 回环采集有两种"这里没声音"的样子:桌面安静时音频引擎**根本不产生数据包**
// (GetNextPacketSize 一直返回 0),以及数据包带着静音标志(引擎在推进时间轴
// 但没有内容)。两种都不该更新这个时刻,于是"静音多久了"一个度量就都盖住了。
//
// 用原子量而不是锁:drain 在锁定的采集线程上写,读它的是一根每两秒醒一次的
// 轮询协程,两者没有需要一起原子看待的字段,不该为此引入一把锁。
type audibility struct {
	// UnixNano。0 表示还没有开始计时 —— 那时 silentFor 返回 0(也就是
	// "不静音"),调用方什么都不要做。宁可漏报也不要在一开始就误报。
	last atomic.Int64
}

// restart 把计时重新锚到此刻,用在一次采集刚开始的时候。
//
// 单独一个方法而不是复用 markAudible:那时候一个声音都还没听到,叫
// "markAudible" 是撒谎,而读代码的人正是靠名字判断这里发生了什么。
// 缺了它,一台从头到尾就没响过的设备会一直报"静音了 0 秒" ——
// 那恰好是这条提示最该管的情况。
func (a *audibility) restart(now time.Time) {
	a.last.Store(now.UnixNano())
}

// markAudible 记下"此刻听到了声音"。
func (a *audibility) markAudible(now time.Time) {
	a.last.Store(now.UnixNano())
}

// silentFor 返回从上次听到声音到现在过了多久。
//
// 从没听到过声音时,答案就是"从开始计时到现在"。
func (a *audibility) silentFor(now time.Time) time.Duration {
	last := a.last.Load()
	if last == 0 {
		return 0
	}
	// 墙上时钟可能被往回拨(对时、夏令时)。负的间隔会让调用方以为
	// "刚响过",这里夹到 0,和没开始计时一个意思。
	if d := now.Sub(time.Unix(0, last)); d > 0 {
		return d
	}
	return 0
}

// Level 是一台播放设备此刻的状态。
//
// 名字和 Default 是给人看的,Peak 是给判定用的。
type Level struct {
	ID   string
	Name string

	// Default 表示这是当前的系统默认播放设备。
	Default bool

	// Peak 是这台端点此刻在渲染的峰值电平,0..1 的线性值。0 表示没有
	// 任何东西在往它上面放 —— 也可能表示这台设备不支持音量表(见
	// capture_windows.go 里 devicePeak 的注释)。
	Peak float32
}

// PickActive 从探到的设备里挑一台值得建议切过去的。
//
// 挑不出来就返回 ok = false,调用方什么都不做。
//
// 规则,按优先级:
//
//   - 跳过正在采的那台 —— 它当然是静音的,不然也不会走到这里;
//   - 跳过峰值低于 eps 的,它们没有在放东西;
//   - 剩下的里面**优先选系统默认那台**。这是唯一一条便宜的启发式:
//     "系统默认指到了一台没在放的设备上"正是这条提示要处理的原始场景,
//     而默认设备通常就是用户以为声音会出来的那台;
//   - 都是"非默认"时,选峰值最大的。
func PickActive(capturedID string, levels []Level, eps float32) (Level, bool) {
	var (
		best     Level
		bestPeak float32
		found    bool
	)
	for _, l := range levels {
		if l.ID == capturedID || l.Peak < eps {
			continue
		}
		switch {
		case !found:
			best, bestPeak, found = l, l.Peak, true
		case l.Default && !best.Default:
			// 默认设备一旦出现就直接赢,不管峰值大小。
			best, bestPeak = l, l.Peak
		case l.Default == best.Default && l.Peak > bestPeak:
			best, bestPeak = l, l.Peak
		}
	}
	return best, found
}
