// Package config 按固定优先级合并 Schema Default、YAML、环境变量和 CLI 覆盖。
package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration 使用 JSON 字符串封装 time.Duration。配置值采用 "500ms"、"2s"、
// "1m30s" 等 Go Duration String。
type Duration struct {
	time.Duration
}

// UnmarshalJSON 解码一个 Go Duration String。
func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode duration string: %w", err)
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", value, err)
	}

	d.Duration = parsed
	return nil
}

// MarshalJSON 把时长编码为 Go Duration String。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}
