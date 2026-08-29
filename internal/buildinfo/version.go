// Package buildinfo 持有由仓库构建入口注入的进程构建元数据。
package buildinfo

const developmentVersion = "(devel)"

// version 由正式 XTunnel Binary 构建使用以下 Linker Flag 替换：
//
//	-X github.com/lifei6671/xtunnel/internal/buildinfo.version=<version>
//
// 本地普通构建保留明确的开发标记。运行时配置、环境变量和 VCS 元数据均不得覆盖
// 该来源；正式构建必须显式注入，以便遗漏时直接暴露为开发版本。
var version = developmentVersion

// Version 返回编译进当前 Binary 的不可变版本。
func Version() string {
	return version
}
