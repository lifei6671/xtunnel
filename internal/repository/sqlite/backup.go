package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	libsqlite "github.com/libtnb/sqlite"
	moderncsqlite "modernc.org/sqlite"
)

// backupStepPages 限制每轮 Online Backup 复制的页数，使大库备份能在页批次之间
// 及时观察 ctx 取消，同时避免逐页调用驱动造成不必要开销。
const backupStepPages int32 = 128

// BackupBarrier 持有 Store 的唯一写租约，是在线维护期间冻结应用写入的 owner。
// 持有期间新写入按 FIFO 排队，普通读取继续运行；SQLite Online Backup 仍从同一
// Store 连接池读取，因此可以包含 WAL 中尚未 checkpoint 的已提交页。
//
// mu 串行化 BackupSQLite 与 Release：释放方必须等正在运行的备份退出，不能在
// 复制中途放行新写入。Release 可重复调用；真正的 writeLease 只归还一次。
type BackupBarrier struct {
	// store 提供受 Barrier 冻结的源连接池。
	store *Store
	// lease 是 Barrier 对 writeGate 唯一租约的所有权凭证。
	lease *writeLease

	mu sync.Mutex
	// released 阻止释放后的 Barrier 再次启动备份。
	released bool
}

// AcquireBackupBarrier 通过 Store 的统一 writeGate 等待当前写事务结束，并阻止
// 新写事务开始。等待过程尊重 ctx 取消，取消后不会在 FIFO 队列中留下占位请求。
// 调用方取得 Barrier 后拥有 Release 责任，通常应立即 defer barrier.Release()。
func (store *Store) AcquireBackupBarrier(ctx context.Context) (*BackupBarrier, error) {
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire SQLite backup barrier: %w", err)
	}
	return &BackupBarrier{store: store, lease: lease}, nil
}

// BackupSQLite 使用 SQLite Online Backup API 把当前 Store 复制成自包含数据库。
// destinationPath 必须不存在；本方法以 O_EXCL 和 0600 创建它，失败或取消时清理
// 主文件及可能产生的 journal/WAL sidecar，避免半成品被后续归档误认成有效快照。
// 本方法不会隐式 Release，Barrier 生命周期仍由取得它的调用方负责。
func (barrier *BackupBarrier) BackupSQLite(ctx context.Context, destinationPath string) error {
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.released {
		return errors.New("SQLite backup barrier is already released")
	}
	return backupSQLiteFromPool(ctx, barrier.store.pool, destinationPath)
}

// Release 归还写租约。若 BackupSQLite 正在运行，Release 会等待备份完成或取消后
// 再放行 FIFO 队首写入；幂等性允许多个清理分支安全汇合。
func (barrier *BackupBarrier) Release() {
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.released {
		return
	}
	barrier.released = true
	barrier.lease.Release()
}

// BackupSQLite 从未迁移的只读源数据库创建自包含副本。只读连接仍由 SQLite 解析
// 主库与 WAL 的一致视图，但本函数不会执行 Migration 或修改源文件。
//
// 调用方必须先持有目标 data-dir 的 External Lock，证明没有运行中的 Server 写入；
// 该锁是离线模式的唯一并发边界，不能仅凭“当前看不到 socket”推断源库静止。
func BackupSQLite(ctx context.Context, sourcePath, destinationPath string) (resultErr error) {
	pool, err := sql.Open(libsqlite.DriverName, readOnlyDatabaseDSN(sourcePath))
	if err != nil {
		return fmt.Errorf("open source SQLite database for backup: %w", err)
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close source SQLite backup pool: %w", err))
		}
	}()
	if err := pool.PingContext(ctx); err != nil {
		return fmt.Errorf("verify source SQLite database for backup: %w", err)
	}
	return backupSQLiteFromPool(ctx, pool, destinationPath)
}

// backupSQLiteFromPool 从既有源连接池执行 SQLite 原生 Online Backup。源池的并发
// 所有权由调用方保证：在线路径持有 BackupBarrier，离线路径持有 External Lock。
// 目标路径先以 O_EXCL 建立候选；只有完整复制成功才返回，任一步失败都会汇总清理
// 错误并移除候选数据库及 sidecar，调用方不能把失败返回后的路径视为已发布备份。
func backupSQLiteFromPool(ctx context.Context, pool *sql.DB, destinationPath string) (resultErr error) {
	if err := createBackupDestination(destinationPath); err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		for _, path := range []string{destinationPath, destinationPath + "-journal", destinationPath + "-wal", destinationPath + "-shm"} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove incomplete SQLite backup %q: %w", path, err))
			}
		}
	}()

	connection, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire source SQLite backup connection: %w", err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close source SQLite backup connection: %w", err))
		}
	}()

	// Online Backup 是驱动连接级 API，必须借助 Raw 固定当前 database/sql 连接，
	// 不能在 Step 循环中把复制状态切换到连接池里的另一条连接。
	err = connection.Raw(func(driverConnection any) (rawErr error) {
		source, ok := driverConnection.(interface {
			NewBackup(string) (*moderncsqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver connection does not support online backup")
		}
		backup, err := source.NewBackup(backupDestinationDSN(destinationPath))
		if err != nil {
			return fmt.Errorf("initialize SQLite online backup: %w", err)
		}
		defer func() {
			if err := backup.Finish(); err != nil {
				rawErr = errors.Join(rawErr, fmt.Errorf("finish SQLite online backup: %w", err))
			}
		}()

		// 分批推进可在批次间传播取消。Finish 无论成功失败都必须执行，以释放
		// 驱动持有的 SQLite backup handle 和目标连接。
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(backupStepPages)
			if err != nil {
				return fmt.Errorf("copy SQLite backup pages: %w", err)
			}
			if !more {
				return nil
			}
		}
	})
	if err != nil {
		return fmt.Errorf("create SQLite online backup: %w", err)
	}
	if err := os.Chmod(destinationPath, 0o600); err != nil {
		return fmt.Errorf("set SQLite backup permissions: %w", err)
	}
	succeeded = true
	return nil
}

// createBackupDestination 用 O_EXCL 预占私有目标名，既拒绝覆盖用户文件，也消除
// “检查后再创建”的竞态。真正的 SQLite 连接随后只以 mode=rw 打开该文件。
func createBackupDestination(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite backup destination %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(
				fmt.Errorf("close SQLite backup destination %q: %w", path, err),
				fmt.Errorf("remove incomplete SQLite backup %q: %w", path, removeErr),
			)
		}
		return fmt.Errorf("close SQLite backup destination %q: %w", path, err)
	}
	return nil
}

// readOnlyDatabaseDSN 构造可读取 SQLite WAL 一致视图、但不能创建或迁移源库的 DSN。
func readOnlyDatabaseDSN(databasePath string) string {
	query := make(url.Values)
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	return sqliteFileDSN(databasePath, query)
}

// immutableDatabaseDSN 用于已完成且不再变化的恢复候选。immutable 避免 SQLite
// 查找或创建 journal/WAL sidecar，调用方必须先通过维护流程保证文件确实静止。
func immutableDatabaseDSN(databasePath string) string {
	query := make(url.Values)
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "foreign_keys(ON)")
	return sqliteFileDSN(databasePath, query)
}

// backupDestinationDSN 只打开已由 O_EXCL 预占的目标。DELETE journal 令成功结果为
// 单一数据库文件，FULL 同步保证返回前 SQLite 已按备份契约提交目标页。
func backupDestinationDSN(databasePath string) string {
	query := make(url.Values)
	query.Set("mode", "rw")
	query.Add("_pragma", "journal_mode(DELETE)")
	query.Add("_pragma", "synchronous(FULL)")
	return sqliteFileDSN(databasePath, query)
}

// sqliteFileDSN 将本地路径编码为 modernc SQLite 接受的 file: URI。先转斜杠再仅
// 转义 Path，可避免 Windows 盘符或特殊字符被误解释为 URI authority/query。
func sqliteFileDSN(databasePath string, query url.Values) string {
	slashPath := filepath.ToSlash(databasePath)
	escapedPath := (&url.URL{Path: slashPath}).EscapedPath()
	return "file:" + escapedPath + "?" + query.Encode()
}
