package server

import (
	"context"
	"io"
	"net/http"
	"time"
)

// 控制页的存活检测:页面关了,程序也跟着退。
//
// 做法是让控制页挂一条 SSE 长连接(/api/alive),页面开着连接就在,页面一关
// 连接就断。这比在页面上用 beforeunload / sendBeacon 报信可靠,两个原因:
//
//   - 刷新页面同样会触发 unload,报信那条路会把"刷新"误判成"关闭"。连接不
//     一样:刷新时旧连接断开、新连接立刻接上,中间那不到一秒的空档用宽限期
//     盖掉就行。
//   - 浏览器崩溃、被任务管理器杀掉时根本来不及发请求,但连接一定会断。
//
// 反过来说,切到别的标签页、锁屏、机器休眠都不会断连接 —— 用户只是切走了一下,
// 不该被当成关了页面。这也正是不能用"页面定时发心跳、超时就退"的原因:浏览器
// 会把后台标签页的定时器降频,挂了很久的页面心跳会稀到几秒甚至一分钟一次,
// 到时候人还在,程序先退了。
//
// 只在曾经有页面连上来过、且宽限期内一个都没回来时,才认定控制页关了。
// 一直没人连(用 -no-browser 启动)不在此列 —— 那不能算"关了"。

const (
	// alivePollInterval 是看门狗检查连接数的间隔。
	alivePollInterval = time.Second

	// aliveGrace 是"页面全没了"到"认定关闭"之间的等待。
	//
	// 刷新一次控制页只要一两百毫秒(页面本身是内嵌的,没有外部请求),
	// 3 秒是十几倍的余量;再长就没必要了 —— 用户关掉标签页之后,程序
	// 该退就得退,不能让人对着一个还在采集的进程等。
	aliveGrace = 3 * time.Second

	// aliveWriteInterval 是往长连接里写保活注释的间隔。
	//
	// 连接是死是活,写一下就知道:页面关掉之后第一次写就会失败,handler
	// 随即返回,连接数正好减回去,不用等 TCP 超时。
	aliveWriteInterval = 2 * time.Second
)

// aliveSnapshot 取当前的控制页连接情况。
func (s *Server) aliveSnapshot() (seen bool, conns int) {
	s.aliveMu.Lock()
	defer s.aliveMu.Unlock()
	return s.aliveSeen, s.aliveConns
}

// controlWatcher 负责"页面全没了"之后的计时。
//
// 单独抽出来是为了能测:真想跑一遍的话,这里的一秒轮询加五秒宽限要跑六秒,
// 而判断本身是纯的 —— 时间喂进去就行。
type controlWatcher struct {
	goneSince time.Time // 页面全没了的时刻;零值表示还有页面开着

	// fired 表示这一轮"开过又关了"已经报告过了。
	//
	// 一轮只报一次。不记这个的话,页面一直关着的时候每过一个宽限期就会
	// 再报一次 —— 而"关页"是一个事件,不是一个持续状态。远程控制开着时
	// 这个区别尤其要紧:那时候关页不退程序,回调会返回"继续盯着",
	// 没有 fired 的话它会每几秒被喊一次。
	fired bool
}

// gone 报告此刻是否可以认定控制页已经关了。
//
// seen 表示曾经有页面连上来过,conns 是当前页面数。
func (w *controlWatcher) gone(now time.Time, seen bool, conns int, grace time.Duration) bool {
	if !seen || conns > 0 {
		// 还有页面开着,或者压根没人连过 —— 两种情况都不算"关了"。
		// 计时一并清零:页面回来过,就得从头再等(也算新的一轮)。
		w.goneSince = time.Time{}
		w.fired = false
		return false
	}
	if w.fired {
		return false
	}
	if w.goneSince.IsZero() {
		w.goneSince = now
		return false
	}
	if now.Sub(w.goneSince) < grace {
		return false
	}
	w.fired = true
	return true
}

// handleAlive 是一条一直挂着的 SSE 连接,控制页开着它就开着。
func (s *Server) handleAlive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	s.aliveMu.Lock()
	s.aliveConns++
	s.aliveSeen = true
	s.aliveMu.Unlock()
	defer func() {
		s.aliveMu.Lock()
		s.aliveConns--
		s.aliveMu.Unlock()
	}()

	t := time.NewTicker(aliveWriteInterval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			// 客户端断开时 Go 的 http 服务器会取消这个 ctx,
			// 这是页面关掉之后最快的一条感知路径。
			return
		case <-t.C:
			// SSE 里以冒号开头的行是注释,浏览器会忽略 —— 写它纯粹是为了
			// 让连接上有流量,顺便把"对端已经没了"这件事暴露出来。
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// watchControlPage 盯着控制页,全都关了且宽限期内没人回来就调一次
// OnControlPageGone。
//
// 回调返回 false 表示它已经收尾了(通常是退程序),看门狗随之结束。
// 返回 true 表示"这次不退,接着盯" —— 远程控制开着时就是这样:关页不退,
// 但用户之后可能把远控关掉再关页,那时候又该退了。一走了之的话,
// 后一种情况就再也没人盯着了。
func (s *Server) watchControlPage(ctx context.Context) {
	var w controlWatcher

	t := time.NewTicker(alivePollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		seen, conns := s.aliveSnapshot()
		if w.gone(time.Now(), seen, conns, aliveGrace) {
			if !s.onControlGone() {
				return
			}
		}
	}
}
