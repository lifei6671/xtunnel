// Package configs 提供嵌入二进制的 Server 配置 Schema。
// JSON Schema 始终是字段元数据、默认值、范围和热加载属性的唯一机器权威。
package configs

import (
	"bytes"
	_ "embed"
)

//go:embed server.schema.json
var serverSchema []byte

// ServerSchema 返回 Server JSON Schema 的独立副本。
func ServerSchema() []byte {
	return bytes.Clone(serverSchema)
}
