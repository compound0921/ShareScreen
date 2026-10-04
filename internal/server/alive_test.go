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
