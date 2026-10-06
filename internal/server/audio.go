package server

import (
	"errors"
	"log"
)

// SetAudioDevice 换掉采集的音频设备,必要的话重启推流。
//
// 右下角那条"采集设备是静音的,要不要换一台"的提示走这条路 —— 控制页里
// 那个下拉最终也落到同一个字段上,两条入口改的是同一份配置。
//
// 存的是**具体设备 ID**,不是空串。空串的含义是"跟随系统默认",而系统
// 默认正是当初指到显示器那一路、把声音采没了的东西 —— 再交回给它等于
// 什么都没改。附带的好处是控制页那个下拉会显示成一台具体设备,而不是
// 一个"自动",用户回头看时知道自己当初选了什么。
func (s *Server) SetAudioDevice(id string) error {
	if id == "" {
		return errors.New("没有指定音频设备")
	}

	before := s.currentConfig()
	if before.Audio.DeviceID == id {
		// 已经是它了。别白重启一次 —— 重启要黑几秒画面。
		return nil
	}

	next := before
	next.Audio.DeviceID = id
	if err := s.updateConfig(next); err != nil {
		return err
	}

	// 推流没在跑就不重启:Restart 不判断状态,它会把推流**拉起来**,
	// 而用户此刻根本没在共享。和 handleConfig 里那条判断同一个道理。
	if s.stream.Status().Running {
		log.Printf("音频设备换成 %q,重启采集", id)
		s.stream.Restart()
	}
	return nil
}
