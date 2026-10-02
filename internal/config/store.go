package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Load 从 path 读取配置。文件不存在时返回默认配置且不报错(首次运行)。
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Default(), fmt.Errorf("读取配置失败: %w", err)
	}

	cfg := Default() // 先铺默认值,再让文件里的字段覆盖 —— 这样新增字段不会因为旧文件缺项而变成零值
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("配置文件格式错误 %s: %w", path, err)
	}
	cfg.Normalize()
	return cfg, nil
}

// Save 原子写入配置。
//
// 先写临时文件再 rename:避免写到一半进程崩溃留下截断的 JSON,
// 那会导致下次启动解析失败、配置全部丢失。
func Save(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}
	// Windows 上 os.Rename 会覆盖已存在的目标
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存配置失败: %w", err)
	}
	return nil
}
