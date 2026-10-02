package proc

import "sync"

// RingBuffer 保存最近写入的若干行文本,供界面展示子进程输出的尾部。
//
// 子进程的输出必须被持续读走 —— 管道缓冲区填满后子进程会阻塞在写操作上,
// 表现为"跑一会儿就卡死"。但完整保留这些输出既无必要也会无限增长,
// 所以只留最后 N 行。
type RingBuffer struct {
	mu   sync.Mutex
	lines []string
	next  int
	full  bool
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{lines: make([]string, capacity)}
}

func (r *RingBuffer) Add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines[r.next] = s
	r.next = (r.next + 1) % len(r.lines)
	if r.next == 0 {
		r.full = true
	}
}

// Last 返回最近 n 行,按时间先后排列(最旧的在前)。
func (r *RingBuffer) Last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	size := r.next
	if r.full {
		size = len(r.lines)
	}
	if n > size {
		n = size
	}
	if n <= 0 {
		return nil
	}

	out := make([]string, 0, n)
	start := r.next - n
	if start < 0 {
		start += len(r.lines)
	}
	for i := 0; i < n; i++ {
		out = append(out, r.lines[(start+i)%len(r.lines)])
	}
	return out
}

// Len 返回当前已保存的行数。
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.lines)
	}
	return r.next
}
