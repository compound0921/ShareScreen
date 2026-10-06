package audio

import (
	"sync"
	"testing"
	"time"
)

// 没开始计时就报"不静音"。宁可漏报,也不要在一开始就把用户吵醒。
func TestAudibilityNotStartedIsNotSilent(t *testing.T) {
	var a audibility
	if got := a.silentFor(time.Now()); got != 0 {
		t.Errorf("还没开始计时,silentFor = %v,想要 0", got)
	}
}

// 从头到尾没响过:静音时长从"开始计时"那一刻长起来。
//
// 这条是这条提示最该管的情况(采集设备从头到尾就是错的),而它恰好
// 依赖 restart 先把锚点打上 —— 少了它,这里会永远返回 0。
func TestAudibilitySilentSinceStart(t *testing.T) {
	var a audibility
	start := time.Now()
	a.restart(start)

	if got := a.silentFor(start.Add(30 * time.Second)); got != 30*time.Second {
		t.Errorf("一直没响过时 silentFor = %v,想要 30s", got)
	}
}

// 响过一次之后,静音时长从那一刻重新算。
func TestAudibilityMarkResets(t *testing.T) {
	var a audibility
	start := time.Now()
	a.restart(start)

	sound := start.Add(5 * time.Second)
	a.markAudible(sound)

	if got := a.silentFor(sound.Add(2 * time.Second)); got != 2*time.Second {
		t.Errorf("响过之后 silentFor = %v,想要 2s", got)
	}
}

// 时钟往回拨(对时、夏令时)不能出现负数 —— 负的间隔会让调用方以为
// "刚响过",把提示压下去。
func TestAudibilityClockBackwards(t *testing.T) {
	var a audibility
	start := time.Now()
	a.restart(start)

	if got := a.silentFor(start.Add(-time.Minute)); got != 0 {
		t.Errorf("时钟倒退时 silentFor = %v,想要 0", got)
	}
}

// 采集线程写、轮询协程读,两个线程同时碰它必须干净。配 -race 跑。
func TestAudibilityConcurrent(t *testing.T) {
	var a audibility
	start := time.Now()
	a.restart(start)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.markAudible(start.Add(time.Duration(i) * time.Millisecond))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			a.silentFor(start.Add(time.Duration(i) * time.Millisecond))
		}
	}()
	wg.Wait()
}

func level(id string, def bool, peak float32) Level {
	return Level{ID: id, Name: id, Default: def, Peak: peak}
}

// 正在采的那台即使峰值很高也要跳过 —— 那是我们自己。
func TestPickActiveSkipsCaptured(t *testing.T) {
	got, ok := PickActive("A", []Level{level("A", false, 0.9)}, 0.001)
	if ok {
		t.Errorf("把正在采的那台挑出来了:%+v", got)
	}
}

// 低于阈值的不算"在放"。这条挡的是空闲设备的底噪和暂停流的残留。
func TestPickActiveIgnoresQuiet(t *testing.T) {
	got, ok := PickActive("A", []Level{
		level("A", false, 0),
		level("B", false, 0.0005),
	}, 0.001)
	if ok {
		t.Errorf("半死不活的那台被挑出来了:%+v", got)
	}
}

// 系统默认那台一旦在放,就赢过峰值更大但不是默认的 —— "默认指到了一台
// 没在放的设备上"正是这条提示要处理的原始场景。
func TestPickActivePrefersDefault(t *testing.T) {
	got, ok := PickActive("A", []Level{
		level("B", false, 0.9),
		level("C", true, 0.05),
	}, 0.001)
	if !ok {
		t.Fatal("应当挑得出设备")
	}
	if got.ID != "C" {
		t.Errorf("挑的是 %q,想要默认那台 C", got.ID)
	}
}

// 没有默认设备在场时,选峰值最大的。
func TestPickActiveFallsBackToLoudest(t *testing.T) {
	got, ok := PickActive("A", []Level{
		level("B", false, 0.2),
		level("C", false, 0.7),
	}, 0.001)
	if !ok {
		t.Fatal("应当挑得出设备")
	}
	if got.ID != "C" {
		t.Errorf("挑的是 %q,想要最响的 C", got.ID)
	}
}
