// Package server 包含服务端应用与运行时组件。
// Session 等运行时状态只存在内存中；持久化状态通过 repository 包访问，不能进入
// 数据面热路径。
package server
