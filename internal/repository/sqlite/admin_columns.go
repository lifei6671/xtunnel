package sqlite

// AdminUserTable 是 admin_users 的固定表名。
const AdminUserTable = "admin_users"

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
