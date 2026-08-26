// Package controlauth 实现 Agent Gateway 的 Server 侧 Control AUTH 边界。
//
// 本包只负责单条已经完成 TLS/ALPN 协商的 xtunnel-control/1 连接：严格读取一个
// ConnectorAuthRequest，校验 Tunnel Token 与协议版本，写回显式认证结果，并在成功
// Frame 完整写出后才把 Session 发布到运行时 Registry。TLS Accept、ALPN 分流以及
// ESTABLISHED 后的 Control Session Owner 由后续 Bootstrap 与 M1-07 负责。
package controlauth
