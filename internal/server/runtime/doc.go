// Package runtime 管理 Server 侧不落库的 Tunnel Connector 与 Control Session 归属。
//
// 本包不执行认证或业务网络读写。每个 TunnelRuntime 使用独立锁线性化 Session、
// Connector Lease 与 ActiveWork；锁内只修改内存状态。ActiveWork 终止时先在锁内
// 摘除，再在锁外按 Cancel、SetDeadline、Close 顺序解除 IO，并用 generation fencing
// 防止旧连接误删新 Session。
package runtime
