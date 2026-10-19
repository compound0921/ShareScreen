package audio

import "testing"

// TestSilencePadBytesAlignsToFrame 钉住静音补齐必须整帧对齐。
//
// 这条曾经是"有音频轨但观众听不到声音"的根因:补齐量不是 BlockAlign
// 的整数倍时,后续所有 PCM 整体错位,ffmpeg 把 f32 读成乱码。
func TestSilencePadBytesAlignsToFrame(t *testing.T) {
	const (
		frame = int64(8) // f32le 立体声:4 字节 × 2 声道
		step  = int64(7680)
	)
	cases := []struct {
		name     string
		want     int64
		produced int64
		out      int64
	}{
		{"刚好够一帧且已对齐", 7680, 0, 7680},
		{"多出 5 字节要向下取整", 7685, 0, 7680},
		{"累计缺口 7999 取整到 7992", 7999, 0, 7992},
		{"不足一步不补", 7679, 0, 0},
		{"已产出超过时钟不补", 5000, 6000, 0},
		{"零头不足一帧不补", 7683, 7680, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := silencePadBytes(c.want, c.produced, frame, step)
			if got != c.out {
				t.Errorf("silencePadBytes(%d,%d) = %d,想要 %d",
					c.want, c.produced, got, c.out)
			}
			if got%frame != 0 {
				t.Errorf("返回值 %d 不是整帧(%d 字节)的倍数", got, frame)
			}
		})
	}
}
