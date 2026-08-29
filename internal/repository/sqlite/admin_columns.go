package sqlite

// AdminUserTable 是 admin_users 的固定表名。
const AdminUserTable = "admin_users"

// AdminSessionTable 是 admin_sessions 的固定表名。
const AdminSessionTable = "admin_sessions"

// AdminUserColumns 集中定义 admin_users 的列名。数据库操作必须通过本结构体
// 引用列名，不得在查询或筛选中重复硬编码字符串。
var AdminUserColumns = struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    string
	UpdatedAt    string
	LastLoginAt  string
}{
	ID:           "id",
	Username:     "username",
	PasswordHash: "password_hash",
	CreatedAt:    "created_at",
	UpdatedAt:    "updated_at",
	LastLoginAt:  "last_login_at",
}

// AdminSessionColumns 集中定义 admin_sessions 的列名，避免认证路径出现漂移的 SQL 字符串。
var AdminSessionColumns = struct {
	ID         string
	UserID     string
	TokenHash  string
	CSRFToken  string
	ExpiresAt  string
	CreatedAt  string
	LastSeenAt string
}{
	ID:         "id",
	UserID:     "user_id",
	TokenHash:  "token_hash",
	CSRFToken:  "csrf_token",
	ExpiresAt:  "expires_at",
	CreatedAt:  "created_at",
	LastSeenAt: "last_seen_at",
}
