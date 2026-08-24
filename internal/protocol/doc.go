// Package protocol 包含 XTunnel 线协议的分帧与校验逻辑。
//
// Protocol v1 契约冻结后，协议消息统一由 api/proto 生成。协议权威留在本包之外，
// 避免 Go 实现意外成为第二份线协议契约。
package protocol
