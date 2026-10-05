package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 「关掉控制页 = 关掉程序」这条行为的全部判断都在 controlWatcher 里,而它
// 一旦判断错,错法有对称的两种:该退不退(关了半天程序还挂着),或者不该退
// 退了(刷新一下页面,正在推的流就没了)。两种都很难在事后复盘,所以这里把
// 每条边界都钉住。
//
// 时间全部喂进去,不真等 —— 真实常量是一秒轮询加五秒宽限,照着跑要六秒。
func TestControlWatcher(t *testing.T) {
	const grace = 5 * time.Second
	base := time.Unix(1700000000, 0)

	t.Run("从没人连过就不算关了", func(t *testing.T) {
		// -no-browser 启动、或者浏览器压根没打开过:这时候把程序退了
		// 就成了"启动即退出"。
		var w controlWatcher
		for _, d := range []time.Duration{0, 5 * time.Second, time.Hour} {
			if w.gone(base.Add(d), false, 0, grace) {
				t.Fatalf("第 %v 秒就认定关闭了", d)
			}
		}
	})

	t.Run("页面还开着就不算关了", func(t *testing.T) {
		var w controlWatcher
		if w.gone(base, true, 1, grace) {
			t.Fatal("还有页面连接时认定关闭")
		}
		if w.gone(base.Add(time.Hour), true, 2, grace) {
			t.Fatal("还有页面连接时认定关闭")
		}
	})

	t.Run("断开后要等满宽限期", func(t *testing.T) {
		var w controlWatcher
		if w.gone(base, true, 0, grace) {
			t.Fatal("刚断开就认定关闭 —— 刷新页面会被误判")
		}
		if w.gone(base.Add(grace-time.Millisecond), true, 0, grace) {
			t.Fatal("还没到宽限期就认定关闭")
		}
		if !w.gone(base.Add(grace), true, 0, grace) {
			t.Fatal("过了宽限期还没认定关闭")
		}
	})

	t.Run("宽限期内连回来就重新计时", func(t *testing.T) {
		// 刷新页面走的正是这条路:断开一下,新页面立刻接上。
		var w controlWatcher
		w.gone(base, true, 0, grace)                         // 断开,开始计时
		if w.gone(base.Add(3*time.Second), true, 1, grace) { // 新页面接上
			t.Fatal("页面已经回来了,不该认定关闭")
		}
		// 计时被清零,所以从这一刻起要再等满一个宽限期
		w.gone(base.Add(4*time.Second), true, 0, grace)
		if w.gone(base.Add(4*time.Second+grace-time.Millisecond), true, 0, grace) {
			t.Fatal("计时没清零,提前认定关闭")
		}
		if !w.gone(base.Add(4*time.Second+grace), true, 0, grace) {
			t.Fatal("重新计时之后满宽限期仍未认定关闭")
		}
	})

	t.Run("一轮只报一次", func(t *testing.T) {
		// 关页是一个事件,不是一个持续状态。
		//
		// 这条在远程控制上是必须的:那时候关页不退程序,回调返回"继续盯着",
		// 看门狗就留在循环里。如果它每过一个宽限期再报一次,程序会每几秒
		// 被喊一次,日志也跟着刷。
		var w controlWatcher
		w.gone(base, true, 0, grace)
		if !w.gone(base.Add(grace), true, 0, grace) {
			t.Fatal("第一次应当认定关闭")
		}
		for i := 1; i <= 3; i++ {
			if w.gone(base.Add(grace+time.Duration(i)*grace), true, 0, grace) {
				t.Fatalf("第 %d 次又报了一遍 —— 一轮应当只报一次", i)
			}
		}
	})

	t.Run("页面重新开过之后能再报一次", func(t *testing.T) {
		// 只报一次不能变成"永远不再报"。用户关掉远控、重开页面、再关页,
		// 这时候就该正常退出了。
		var w controlWatcher
		w.gone(base, true, 0, grace)
		w.gone(base.Add(grace), true, 0, grace)

		if w.gone(base.Add(2*grace), true, 1, grace) {
			t.Fatal("页面回来了,不该认定关闭")
		}
		w.gone(base.Add(3*grace), true, 0, grace)
		if !w.gone(base.Add(3*grace+grace), true, 0, grace) {
			t.Fatal("页面重新开过又关掉,应当能再报一次")
		}
	})
}

// 连接数数不回来,页面关掉之后程序就永远不会退 —— 这是整套判断的地基,
// 而它靠的是 http 服务器在客户端断开时取消请求 ctx。
func TestAliveConnTracked(t *testing.T) {
	s := &Server{}
	ts := httptest.NewServer(http.HandlerFunc(s.handleAlive))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q,想要 text/event-stream", ct)
	}

	// 读到第一次保活写入,说明这条连接确实建立起来了。
	// 不能只发请求就数数 —— 那样子请求还没进到 handler。
	buf := make([]byte, 32)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}

	if seen, conns := s.aliveSnapshot(); !seen || conns != 1 {
		t.Fatalf("连上之后 seen=%v conns=%d,想要 true/1", seen, conns)
	}

	// 关掉页面等价于关掉这条连接
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, conns := s.aliveSnapshot(); conns == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("连接关了但计数没减回去,页面的死活就判断不出来了")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
