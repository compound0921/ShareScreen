// Package clipboard 在主机和远处的控制者之间同步剪贴板文本。
//
// # 只做文本
//
// 剪贴板里能放的东西很多,这里只处理 CF_UNICODETEXT。图片和文件是另一种
// 格式,体积经常到几 MB —— 经 WebSocket 送过去会把连接堵住(控制通道和
// 输入事件共用同一条连接,堵住它的后果是鼠标也跟着卡),而收益远不如
// 传一段文本。
//
// # 不需要 LockOSThread
//
// 剪贴板 API 不是 COM,没有线程单元的要求。internal/audio 里那个
// runtime.LockOSThread 是 COM 的要求,别照搬到这里。
//
// 但**必须串行化**:两个 OpenClipboard 同时进行会互相失败。这里用一把
// 锁把所有剪贴板操作串起来,调用方从任意 goroutine 调都可以。
//
// # 防回环
//
// 主机把文本推给控制者之后,控制者那边往往又会把它送回来("我这边剪贴板
// 变了")。没有抑制的话两边会来回传同一段文本,直到某一方察觉到内容没变。
// 靠记住"我们刚写进去的那段文本的哈希"来断掉这个环。
package clipboard

import (
	"hash/fnv"
	"sync"
)

// MaxText 是同步文本的长度上限。
//
// 剪贴板里经常躺着整篇文档。超限就整个跳过,而不是截断 —— 送过去半篇
// 比不送更糟,用户会以为复制成功了。
const MaxText = 256 * 1024

// Sync 是剪贴板同步的状态。
type Sync struct {
	mu sync.Mutex

	// seq 是上一次看到的系统剪贴板序列号。内容一变它就变,靠它判断
	// "变没变"比每次读一遍内容便宜得多。
	seq    uint32
	primed bool

	// lastApplied 是我们自己刚写进剪贴板的文本的哈希,用来断回环。
	lastApplied uint64
}

func New() *Sync { return &Sync{} }

// Read 读本机剪贴板里的文本。
//
// changed 为 false 表示"没有新的东西要同步" —— 可能是内容没变、可能是
// 太长、也可能读的时候被别的进程占着。这几种情况调用方都只需要跳过。
func (s *Sync) Read() (text string, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seq := sequence()
	if s.primed && seq == s.seq {
		return "", false
	}
	s.primed, s.seq = true, seq

	text, err := readText()
	if err != nil {
		// 读失败不算错误:别的进程正占着剪贴板是常态(打开一个
		// 复制对话框的时候)。跳过这一次,下次轮询再来。
		return "", false
	}
	if text == "" || len(text) > MaxText {
		return "", false
	}
	if hash(text) == s.lastApplied {
		return "", false // 我们自己刚写进去的,别再推回去
	}
	return text, true
}

// Current 读当前剪贴板内容,不管它变没变。
//
// 专门给"控制者刚拿到控制权"那一刻用。轮询是**只在变化时**通知的,
// 所以主机的剪贴板里如果本来就躺着一段内容,新连上来的人永远等不到它 ——
// 除非主机碰巧又复制了一次别的东西。而实际情况是:复制东西过去通常
// 发生在建立控制之前("我先复制好再连过去"),正好落在那个死角里。
func (s *Sync) Current() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	text, err := readText()
	if err != nil || text == "" || len(text) > MaxText {
		return ""
	}
	return text
}

// Write 把控制者送来的文本写进本机剪贴板。
func (s *Sync) Write(text string) error {
	if text == "" || len(text) > MaxText {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := writeText(text); err != nil {
		return err
	}

	// 序列号必须在这里就更新。不更新的话,下一次 Read 会立刻把刚写进去的
	// 内容当成"变了",转头又推回给控制者 —— 正好是要防的那个环。
	s.lastApplied = hash(text)
	s.seq = sequence()
	s.primed = true
	return nil
}

func hash(text string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return h.Sum64()
}
