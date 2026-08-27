package durableops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maxManifestSize 限制不可信归档在 JSON 解码前的内存占用。
const maxManifestSize = 1 << 20

// CreateOptions 描述一次备份捕获。BackupDatabase 必须把 SQLite
// Backup API 产生的自包含数据库写到 destination，并返回当时的 Schema 版本。
type CreateOptions struct {
	// DataDir 是已固定身份的稳定数据叶目录。
	DataDir string
	// TLSMode 决定是否捕获本地 Gateway Identity。
	TLSMode TLSMode
	// OutputPath 是绝对输出路径；Create 以独占创建语义拒绝覆盖。
	OutputPath     string
	BackupDatabase func(ctx context.Context, destination string) (schemaVersion int, err error)
	// BeforePublish 在归档已完整落盘、但尚未承诺输出成功前执行。在线备份用它
	// 完成 Barrier Release ACK；租约丢失时返回错误，Create 会删除半成品。
	BeforePublish func() error
}

// Create 使用 0700 临时目录捕获数据边界，并以 O_CREATE|O_EXCL、0600
// 写出备份。失败时删除不完整输出，不会覆盖已有文件。
func Create(ctx context.Context, options CreateOptions) (result Manifest, resultErr error) {
	if !platformSupported() {
		return Manifest{}, ErrUnsupported
	}
	if ctx == nil {
		return Manifest{}, errors.New("backup context is nil")
	}
	if options.BackupDatabase == nil {
		return Manifest{}, errors.New("SQLite backup callback is nil")
	}
	if options.DataDir == "" || options.OutputPath == "" || !filepath.IsAbs(options.DataDir) || !filepath.IsAbs(options.OutputPath) {
		return Manifest{}, errors.New("backup data and output paths must be absolute")
	}
	if options.TLSMode != TLSModePinned && options.TLSMode != TLSModePublic {
		return Manifest{}, fmt.Errorf("backup TLS mode %q is invalid", options.TLSMode)
	}

	snapshot, err := os.MkdirTemp("", "xtunnel-backup-")
	if err != nil {
		return Manifest{}, fmt.Errorf("create backup snapshot directory: %w", err)
	}
	if err := os.Chmod(snapshot, 0o700); err != nil {
		cleanupErr := os.RemoveAll(snapshot)
		return Manifest{}, errors.Join(fmt.Errorf("set backup snapshot permissions: %w", err), cleanupErr)
	}
	defer func() {
		// snapshot 只承载本次进程创建的副本，不是 Stable Target，可用 RemoveAll；
		// 恢复目录的删除必须走带 mount fencing 的 removeDirectoryTree。
		if snapshot != "" {
			if err := os.RemoveAll(snapshot); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove backup snapshot directory: %w", err))
			}
		}
	}()

	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	databasePath := filepath.Join(snapshot, "xtunnel.db")
	schemaVersion, err := options.BackupDatabase(ctx, databasePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("capture SQLite backup: %w", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		return Manifest{}, fmt.Errorf("set SQLite backup permissions: %w", err)
	}
	if options.TLSMode == TLSModePinned {
		for _, relative := range []string{
			"pki/agent-gateway.rotation.json",
			"pki/agent-gateway.key.rotate",
			"pki/agent-gateway.crt.rotate",
		} {
			if _, err := os.Lstat(filepath.Join(options.DataDir, filepath.FromSlash(relative))); err == nil {
				return Manifest{}, fmt.Errorf("backup requires reconciled Gateway identity; pending artifact %q exists", relative)
			} else if !errors.Is(err, os.ErrNotExist) {
				return Manifest{}, fmt.Errorf("inspect pending Gateway identity artifact %q: %w", relative, err)
			}
		}
	}
	for _, rule := range archiveFileRules[1:] {
		if rule.pinned && options.TLSMode != TLSModePinned {
			continue
		}
		if err := copySnapshotFile(ctx, options.DataDir, snapshot, rule); err != nil {
			if errors.Is(err, os.ErrNotExist) && !rule.required {
				continue
			}
			return Manifest{}, err
		}
	}

	output, outputParent, err := createExclusiveOutput(options.OutputPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("create backup output: %w", err)
	}
	completed := false
	defer func() {
		// outputParent 是创建时固定的目录 FD。失败清理必须相对它 unlink，不能重新
		// 解析 OutputPath，否则父路径被并发替换后可能误删另一目录的同名文件。
		if output != nil {
			if err := output.Close(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("close backup output: %w", err))
				completed = false
			}
		}
		if !completed {
			if err := removeExclusiveOutput(outputParent, filepath.Base(options.OutputPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove failed backup output: %w", err))
			}
			if err := outputParent.Sync(); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("sync backup output parent after cleanup: %w", err))
			}
		}
		if err := outputParent.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close backup output parent: %w", err))
		}
	}()
	if err := output.Chmod(0o600); err != nil {
		return Manifest{}, fmt.Errorf("set backup output permissions: %w", err)
	}
	manifest, _, err := createArchive(ctx, output, snapshot, schemaVersion, options.TLSMode)
	if err != nil {
		return Manifest{}, err
	}
	if err := output.Sync(); err != nil {
		return Manifest{}, fmt.Errorf("sync backup output: %w", err)
	}
	if err := output.Close(); err != nil {
		return Manifest{}, fmt.Errorf("close backup output before publishing completion: %w", err)
	}
	output = nil
	if err := os.RemoveAll(snapshot); err != nil {
		return Manifest{}, fmt.Errorf("remove backup snapshot directory: %w", err)
	}
	snapshot = ""
	if err := outputParent.Sync(); err != nil {
		return Manifest{}, fmt.Errorf("sync backup output parent: %w", err)
	}
	if options.BeforePublish != nil {
		// 在线模式的 Barrier Release ACK 是发布承诺的一部分；ACK 失败表示调用方
		// 无法确认捕获边界仍成立，此时 defer 会删除已写完但未承诺的归档。
		if err := options.BeforePublish(); err != nil {
			return Manifest{}, fmt.Errorf("confirm backup publication barrier: %w", err)
		}
	}
	completed = true
	return manifest, nil
}

// createArchive 先从快照构造并验证 Manifest，再按固定顺序写 canonical USTAR。
// Manifest 位于首条目，恢复端可在创建任何业务文件前确定完整白名单和资源上限。
func createArchive(ctx context.Context, output io.Writer, sourceDir string, schemaVersion int, tlsMode TLSMode) (Manifest, string, error) {
	if output == nil {
		return Manifest{}, "", errors.New("backup output is nil")
	}
	if schemaVersion < 1 {
		return Manifest{}, "", errors.New("backup schema version must be positive")
	}
	if tlsMode != TLSModePinned && tlsMode != TLSModePublic {
		return Manifest{}, "", fmt.Errorf("backup TLS mode %q is invalid", tlsMode)
	}

	type sourceFile struct {
		manifest ManifestFile
		path     string
	}
	files := make([]sourceFile, 0, len(archiveFileRules))
	for _, rule := range archiveFileRules {
		if rule.pinned && tlsMode != TLSModePinned {
			continue
		}
		path := filepath.Join(sourceDir, filepath.FromSlash(rule.path))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && !rule.required {
			continue
		}
		if err != nil {
			return Manifest{}, "", fmt.Errorf("inspect backup source %q: %w", rule.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return Manifest{}, "", fmt.Errorf("backup source %q must be a regular non-symbolic-link file", rule.path)
		}
		if !sourceModeValid(info.Mode(), rule.mode) {
			return Manifest{}, "", fmt.Errorf("backup source %q mode is %04o, want %04o", rule.path, info.Mode().Perm(), rule.mode)
		}
		digest, size, err := digestFile(path, info)
		if err != nil {
			return Manifest{}, "", fmt.Errorf("digest backup source %q: %w", rule.path, err)
		}
		files = append(files, sourceFile{
			manifest: ManifestFile{Path: rule.path, Size: size, Mode: rule.mode, SHA256: digest},
			path:     path,
		})
	}

	manifest := Manifest{FormatVersion: FormatVersion, SchemaVersion: schemaVersion, TLSMode: tlsMode}
	for _, file := range files {
		manifest.Files = append(manifest.Files, file.manifest)
	}
	if err := validateManifest(manifest, schemaVersion); err != nil {
		return Manifest{}, "", err
	}
	if err := validateRestoredState(ctx, sourceDir, manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("validate captured backup state: %w", err)
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("marshal backup manifest: %w", err)
	}
	manifestDigest := sha256.Sum256(manifestData)

	archive := tar.NewWriter(output)
	closeArchive := func(cause error) error {
		if err := archive.Close(); err != nil {
			return errors.Join(cause, fmt.Errorf("close backup archive: %w", err))
		}
		return cause
	}
	if err := archive.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(manifestData)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return Manifest{}, "", closeArchive(fmt.Errorf("write backup manifest header: %w", err))
	}
	if _, err := archive.Write(manifestData); err != nil {
		return Manifest{}, "", closeArchive(fmt.Errorf("write backup manifest: %w", err))
	}
	for _, file := range files {
		if err := archive.WriteHeader(&tar.Header{Name: file.manifest.Path, Mode: int64(file.manifest.Mode), Size: file.manifest.Size, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
			return Manifest{}, "", closeArchive(fmt.Errorf("write backup file %q header: %w", file.manifest.Path, err))
		}
		if err := copySourceFile(archive, file.path, file.manifest); err != nil {
			return Manifest{}, "", closeArchive(err)
		}
	}
	if err := archive.Close(); err != nil {
		return Manifest{}, "", fmt.Errorf("close backup archive: %w", err)
	}
	return manifest, hex.EncodeToString(manifestDigest[:]), nil
}

// copySnapshotFile 通过固定在 sourceRoot 下的 FD 捕获一个敏感文件。
// 复制前后比较 inode 与长度，可检测路径替换或长度变化；同 inode、同长度的就地
// 改写必须由调用方持有的 External Lock 或 BackupBarrier 排除。
func copySnapshotFile(ctx context.Context, sourceRoot, snapshotRoot string, rule archiveFileRule) (resultErr error) {
	source, err := openRegularBeneath(sourceRoot, rule.path)
	if err != nil {
		return fmt.Errorf("open backup source %q: %w", rule.path, err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close backup source %q: %w", rule.path, err))
		}
	}()
	before, err := source.Stat()
	if err != nil {
		return fmt.Errorf("inspect backup source %q: %w", rule.path, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("backup source %q must be a regular file", rule.path)
	}
	if !sourceModeValid(before.Mode(), rule.mode) {
		return fmt.Errorf("backup source %q mode is %04o, want %04o", rule.path, before.Mode().Perm(), rule.mode)
	}
	after, err := source.Stat()
	if err != nil || !os.SameFile(before, after) {
		return fmt.Errorf("backup source %q changed before capture", rule.path)
	}
	destinationPath := filepath.Join(snapshotRoot, filepath.FromSlash(rule.path))
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o700); err != nil {
		return fmt.Errorf("create backup snapshot directory: %w", err)
	}
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(rule.mode))
	if err != nil {
		return fmt.Errorf("create backup snapshot file %q: %w", rule.path, err)
	}
	written, copyErr := io.Copy(destination, &contextReader{ctx: ctx, reader: source})
	afterCopy, statErr := source.Stat()
	chmodErr := destination.Chmod(os.FileMode(rule.mode))
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if err := errors.Join(copyErr, statErr, chmodErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("capture backup source %q: %w", rule.path, err)
	}
	if !os.SameFile(before, afterCopy) || written != before.Size() {
		return fmt.Errorf("backup source %q changed during capture", rule.path)
	}
	return nil
}

// contextReader 在每次底层读取前传播取消；它不拥有 reader，也不负责关闭。
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// countingReader 记录 tar 解码器实际消费的字节，用于拒绝归档尾部隐藏数据。
type countingReader struct {
	reader io.Reader
	read   int64
}

// Read 代理读取并累计真实返回的字节数，包括与 EOF 同次返回的数据。
func (reader *countingReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.read += int64(count)
	return count, err
}

// Read 在进入可能阻塞的底层读取前检查 Context；底层文件由调用方关闭解阻塞。
func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// digestFile 使用 no-follow FD 计算摘要，并与调用方捕获的打开前身份比较。
// 文件身份、类型或长度在读取期间变化时，摘要不作为可信 Manifest 数据返回。
func digestFile(path string, before os.FileInfo) (string, int64, error) {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return "", 0, err
	}
	digest := sha256.New()
	size, copyErr := io.Copy(digest, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if statErr != nil {
		return "", 0, statErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if !os.SameFile(before, after) || !after.Mode().IsRegular() || size != before.Size() {
		return "", 0, errors.New("backup source changed while being read")
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

// copySourceFile 把快照文件写入归档，同时重算摘要和长度。
// 这道二次校验保证 Manifest 创建后发生的修改不会被静默写入归档。
func copySourceFile(output io.Writer, path string, manifest ManifestFile) error {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return fmt.Errorf("open backup source %q: %w", manifest.Path, err)
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(output, digest), file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write backup source %q: %w", manifest.Path, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close backup source %q: %w", manifest.Path, closeErr)
	}
	if written != manifest.Size || hex.EncodeToString(digest.Sum(nil)) != manifest.SHA256 {
		return fmt.Errorf("backup source %q changed after manifest creation", manifest.Path)
	}
	return nil
}

// readManifest 严格读取归档首条目并拒绝未知 JSON 字段、尾随 JSON 值和超限内容。
// 返回原始 JSON 字节，使 Journal 绑定的是归档实际声明而非重新编码前的输入流。
func readManifest(archive *tar.Reader, currentSchemaVersion int) (Manifest, []byte, error) {
	header, err := archive.Next()
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read backup manifest header: %w", err)
	}
	if header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Mode != 0o600 || header.Size < 0 || header.Size > maxManifestSize {
		return Manifest{}, nil, errors.New("backup archive must begin with a regular 0600 manifest.json")
	}
	data, err := io.ReadAll(io.LimitReader(archive, maxManifestSize+1))
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read backup manifest: %w", err)
	}
	if int64(len(data)) != header.Size {
		return Manifest{}, nil, errors.New("backup manifest size does not match its header")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("parse backup manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, nil, err
	}
	if err := validateManifest(manifest, currentSchemaVersion); err != nil {
		return Manifest{}, nil, err
	}
	return manifest, data, nil
}

// ensureJSONEOF 要求一个 JSON 文档后只剩空白，避免第二个值绕过严格字段校验。
func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("backup manifest contains trailing JSON values")
		}
		return fmt.Errorf("parse trailing backup manifest data: %w", err)
	}
	return nil
}

// validateArchiveHeader 将每个 USTAR Header 与 Manifest 精确绑定。
// 路径、类型、权限和长度任一不符都在创建目标文件前失败。
func validateArchiveHeader(expected ManifestFile, header *tar.Header) error {
	if header.Name != expected.Path || header.Typeflag != tar.TypeReg || header.Format != tar.FormatUSTAR || header.Size != expected.Size || uint32(header.Mode) != expected.Mode {
		return fmt.Errorf("backup entry %q does not match manifest file %q", header.Name, expected.Path)
	}
	return nil
}
