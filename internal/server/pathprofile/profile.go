// Package pathprofile 解析 Server 持久化数据与运行时目录的同一身份边界。
package pathprofile

// AutomaticDataDir 是 Server 配置中请求当前平台前台默认数据目录的唯一值。
// 它不是路径展开语法；平台实现将其解析为受控 Known Folder 下的绝对目录。
const AutomaticDataDir = "auto"

// Profile 把同一运行身份可访问的 Data 与 Runtime 固定为不可混用的一组路径。
// SQLite、密钥、Journal 与 External Lock 必须始终来自同一个 Profile。
type Profile struct {
	DataDir    string
	RuntimeDir string
	// ManagedRoot 是当前 Profile 可创建并施加保护 ACL 的首个目录。前台
	// Profile 绝不接管用户家目录根；服务 Profile 同样不接管 ProgramData。
	ManagedRoot string
}

// Resolve 将前台配置值解析为当前用户可访问的 Server 路径组。
func Resolve(dataDir string) (Profile, error) {
	return resolveForeground(dataDir)
}

// ResolveService 将服务配置值解析为受管 Service 身份的独立路径组。
// M8-05 的 SCM 入口必须调用它，不能复用前台目录。
func ResolveService(dataDir string) (Profile, error) {
	return resolveService(dataDir)
}
