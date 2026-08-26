// Package identity 管理 XTunnel 的标识格式与短期运行时身份。
//
// Connector 和 Session 都不能写入磁盘、数据库或目录锁：前者在
// xtunnel-agent 进程启动时生成并在全部重连间保持，后者只在
// Server 完成认证后生成。Tunnel 是持久化的逻辑负载单元，本包只校验其 ID。
package identity
