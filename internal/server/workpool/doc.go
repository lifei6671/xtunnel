// Package workpool 实现 Server 侧按 Session 隔离的 WorkConn 内存池与 Demand 裁决。
//
// 本包只管理内存状态和连接所有权，不读写协议 Frame，也不依赖 Control Owner、
// WorkHello Authenticator 或 Tunnel Runtime。上层负责把 DemandHandoff 同时交给
// Control Outbox 与 Budget Lease Authenticator，并在对应协议成功点调用状态转换。
package workpool
