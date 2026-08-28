// Package route 从 SQLite 权威 Desired State 构建并原子发布只读路由快照。
//
// 公网请求只读取内存快照，绝不在热路径查询 SQLite。持久化状态的变更通过
// Manager.MarkDirty 合并唤醒唯一调和 owner，由它重建完整候选、执行 generation
// fencing，并在候选仍对应最新权威状态时一次性发布。
package route
