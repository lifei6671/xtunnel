// Package tunnel 负责与具体传输无关的隧道开启和字节流转发。
//
// 本包不依赖公网入口或具体网络传输，使同一套隧道生命周期可以服务 HTTP、
// WebSocket 和原始 TCP 流量。
package tunnel
