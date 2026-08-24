// Package datadir 负责 Server 数据目录的稳定寻址和获锁后校验。
package datadir

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// Target 描述不依赖 leaf 当前是否存在的稳定数据目标。
type Target struct {
	Path   string
	Parent string
	Leaf   string
	Hash   string
}

// Resolve 使用 realpath(parent) + basename(dataDir) 计算稳定数据目标。
// 该阶段只访问父目录，不读取、创建或解析正式数据目录 leaf。
func Resolve(dataDir string) (Target, error) {
	if !filepath.IsAbs(dataDir) {
		return Target{}, fmt.Errorf("server data directory must be absolute: %q", dataDir)
	}

	// 只移除末尾分隔符，不在 realpath 前折叠路径。含“.”或“..”的输入
	// 会在符号链接祖先下产生歧义，因此作为非规范 Stable Target 直接拒绝。
	targetPath := dataDir
	volumeLength := len(filepath.VolumeName(targetPath))
	rootLength := volumeLength
	if len(targetPath) > volumeLength && os.IsPathSeparator(targetPath[volumeLength]) {
		rootLength++
	}
	for len(targetPath) > rootLength && os.IsPathSeparator(targetPath[len(targetPath)-1]) {
		targetPath = targetPath[:len(targetPath)-1]
	}
	componentStart := 0
	for index := 0; index <= len(targetPath); index++ {
		if index < len(targetPath) && !os.IsPathSeparator(targetPath[index]) {
			continue
		}
		component := targetPath[componentStart:index]
		if component == "." || component == ".." {
			return Target{}, fmt.Errorf("server data directory must not contain dot path components: %q", dataDir)
		}
		componentStart = index + 1
	}
	separatorIndex := -1
	for index := len(targetPath) - 1; index >= 0; index-- {
		if os.IsPathSeparator(targetPath[index]) {
			separatorIndex = index
			break
		}
	}
	if separatorIndex < 0 {
		return Target{}, fmt.Errorf("server data directory must name a leaf directory: %q", dataDir)
	}
	leaf := targetPath[separatorIndex+1:]
	if leaf == "" || leaf == "." || leaf == ".." {
		return Target{}, fmt.Errorf("server data directory must name a leaf directory: %q", dataDir)
	}
	parent := targetPath[:separatorIndex]
	if separatorIndex == volumeLength {
		parent = targetPath[:separatorIndex+1]
	}

	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return Target{}, fmt.Errorf("inspect server data parent %q: %w", parent, err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 {
		return Target{}, fmt.Errorf("server data parent %q must not be a symbolic link", parent)
	}
	if !parentInfo.IsDir() {
		return Target{}, fmt.Errorf("server data parent %q is not a directory", parent)
	}

	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return Target{}, fmt.Errorf("resolve server data parent %q: %w", parent, err)
	}
	canonicalParent, err = filepath.Abs(canonicalParent)
	if err != nil {
		return Target{}, fmt.Errorf("make server data parent absolute: %w", err)
	}

	stablePath := filepath.Join(canonicalParent, leaf)
	hash := sha256.Sum256([]byte(stablePath))
	return Target{
		Path:   stablePath,
		Parent: canonicalParent,
		Leaf:   leaf,
		Hash:   fmt.Sprintf("%x", hash),
	}, nil
}
