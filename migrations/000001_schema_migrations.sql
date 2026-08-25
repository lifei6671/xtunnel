CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

-- 管理 Management 平台访问身份的管理员账户；SQLite 不持久化 COMMENT，
-- 因此字段语义在本 Migration 和 AdminUser 映射结构体中共同维护。
CREATE TABLE admin_users (
    -- 不可变管理员标识。
    id TEXT PRIMARY KEY,

    -- 管理员登录名；系统内全局唯一。
    username TEXT NOT NULL UNIQUE,

    -- Argon2id PHC 编码后的密码哈希，绝不保存明文。
    password_hash TEXT NOT NULL,

    -- 账户创建与最近资料更新时间，均为 UTC Unix 秒。
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,

    -- NULL 表示该用户尚未完成过成功登录。
    last_login_at INTEGER
);
