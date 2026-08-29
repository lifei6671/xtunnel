package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/migrations"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestFirstAdminLifecycle(t *testing.T) {
	if got := (AdminUser{}).TableName(); got != AdminUserTable {
		t.Fatalf("AdminUser.TableName() = %q, want %q", got, AdminUserTable)
	}
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		t.Fatalf("HasAdmin() before create error = %v", err)
	}
	if hasAdmin {
		t.Fatal("HasAdmin() before create = true, want SETUP_REQUIRED")
	}
	if err := store.CreateFirstAdmin(context.Background(), "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}
	hasAdmin, err = store.HasAdmin(context.Background())
	if err != nil {
		t.Fatalf("HasAdmin() after create error = %v", err)
	}
	if !hasAdmin {
		t.Fatal("HasAdmin() after create = false")
	}

	var admin struct {
		PasswordHash string
		LastLoginAt  *int64
	}
	if err := store.database.Table(AdminUserTable).
		Select(AdminUserColumns.PasswordHash, AdminUserColumns.LastLoginAt).
		Where(clause.Eq{Column: AdminUserColumns.Username, Value: "admin"}).
		Scan(&admin).Error; err != nil {
		t.Fatalf("read stored password hash error = %v", err)
	}
	if admin.LastLoginAt != nil {
		t.Fatalf("first admin last_login_at = %d, want NULL", *admin.LastLoginAt)
	}
	if admin.PasswordHash == "correct horse battery staple" || !strings.HasPrefix(admin.PasswordHash, "$argon2id$v=19$m=65536,t=3,p=") {
		t.Fatalf("stored password hash has unexpected format")
	}
	verified, err := verifyPassword(admin.PasswordHash, "correct horse battery staple")
	if err != nil || !verified {
		t.Fatalf("verifyPassword(correct password) = %t, %v", verified, err)
	}
	verified, err = verifyPassword(admin.PasswordHash, "incorrect password")
	if err != nil || verified {
		t.Fatalf("verifyPassword(incorrect password) = %t, %v", verified, err)
	}
	if err := store.CreateFirstAdmin(context.Background(), "other-admin", "another password"); !errors.Is(err, ErrAdminAlreadyExists) {
		t.Fatalf("second CreateFirstAdmin() error = %v, want ErrAdminAlreadyExists", err)
	}
}

func TestCreateFirstAdminAllowsOnlyOneConcurrentWinner(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, username := range []string{"admin-one", "admin-two"} {
		go func(username string) {
			start.Wait()
			results <- store.CreateFirstAdmin(context.Background(), username, "concurrent password")
		}(username)
	}
	start.Done()

	created := 0
	alreadyInitialized := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrAdminAlreadyExists):
			alreadyInitialized++
		default:
			t.Fatalf("concurrent CreateFirstAdmin() error = %v", err)
		}
	}
	if created != 1 || alreadyInitialized != 1 {
		t.Fatalf("concurrent results = created %d, already initialized %d; want 1, 1", created, alreadyInitialized)
	}
}

func TestCreateFirstAdminRejectsEmptyCredentials(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.CreateFirstAdmin(context.Background(), "", "password"); err == nil {
		t.Fatal("CreateFirstAdmin() accepted an empty username")
	}
	if err := store.CreateFirstAdmin(context.Background(), "admin", ""); err == nil {
		t.Fatal("CreateFirstAdmin() accepted an empty password")
	}
}

func TestAdminCredentialsAndSessionLifecycle(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.CreateFirstAdmin(context.Background(), "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}
	for _, credentials := range []struct {
		username string
		password string
	}{
		{username: "missing", password: "correct horse battery staple"},
		{username: "admin", password: "wrong password"},
	} {
		if _, err := store.VerifyAdminCredentials(context.Background(), credentials.username, credentials.password); !errors.Is(err, repository.ErrInvalidAdminCredentials) {
			t.Fatalf("VerifyAdminCredentials(%q) error = %v, want ErrInvalidAdminCredentials", credentials.username, err)
		}
	}
	admin, err := store.VerifyAdminCredentials(context.Background(), "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyAdminCredentials(correct) error = %v", err)
	}
	if !identity.ValidAdminID(admin.ID) || admin.Username != "admin" {
		t.Fatalf("verified admin = %#v", admin)
	}

	sessionID, err := identity.NewAdminSessionID()
	if err != nil {
		t.Fatalf("NewAdminSessionID() error = %v", err)
	}
	tokenHash := sha256.Sum256([]byte("cookie-token-never-persisted"))
	var csrfToken [32]byte
	for index := range csrfToken {
		csrfToken[index] = byte(index + 1)
	}
	const createdAt int64 = 1_800_000_000
	session := repository.AdminSession{
		ID:         sessionID,
		UserID:     admin.ID,
		TokenHash:  tokenHash,
		CSRFToken:  csrfToken,
		ExpiresAt:  createdAt + 3600,
		CreatedAt:  createdAt,
		LastSeenAt: createdAt,
	}
	if err := store.CreateAdminSession(context.Background(), session, createdAt); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}

	got, err := store.GetAdminSessionByTokenHash(context.Background(), tokenHash, createdAt+1, createdAt-1800)
	if err != nil {
		t.Fatalf("GetAdminSessionByTokenHash() error = %v", err)
	}
	if got.ID != session.ID || got.UserID != admin.ID || got.Admin != admin || got.TokenHash != tokenHash || got.CSRFToken != csrfToken {
		t.Fatalf("stored session = %#v", got)
	}
	duplicateID, err := identity.NewAdminSessionID()
	if err != nil {
		t.Fatal(err)
	}
	duplicate := session
	duplicate.ID = duplicateID
	duplicate.CSRFToken[0]++
	if err := store.CreateAdminSession(context.Background(), duplicate, createdAt); err == nil {
		t.Fatal("CreateAdminSession() accepted duplicate token hash")
	}
	var persisted struct {
		TokenHash []byte `gorm:"column:token_hash"`
	}
	if err := store.database.Table(AdminSessionTable).Select(AdminSessionColumns.TokenHash).
		Where(AdminSessionColumns.ID+" = ?", sessionID).Scan(&persisted).Error; err != nil {
		t.Fatalf("read persisted token hash error = %v", err)
	}
	if string(persisted.TokenHash) == "cookie-token-never-persisted" || !bytes.Equal(persisted.TokenHash, tokenHash[:]) {
		t.Fatal("database did not preserve only the cookie token SHA-256")
	}

	if err := store.TouchAdminSession(context.Background(), sessionID, createdAt+10); err != nil {
		t.Fatalf("TouchAdminSession(newer) error = %v", err)
	}
	if err := store.TouchAdminSession(context.Background(), sessionID, createdAt+5); err != nil {
		t.Fatalf("TouchAdminSession(older) error = %v", err)
	}
	got, err = store.GetAdminSessionByTokenHash(context.Background(), tokenHash, createdAt+11, createdAt)
	if err != nil {
		t.Fatalf("GetAdminSessionByTokenHash(after touch) error = %v", err)
	}
	if got.LastSeenAt != createdAt+10 {
		t.Fatalf("last_seen_at = %d, want %d", got.LastSeenAt, createdAt+10)
	}
	if _, err := store.GetAdminSessionByTokenHash(context.Background(), tokenHash, session.ExpiresAt, createdAt); !errors.Is(err, repository.ErrAdminSessionExpired) {
		t.Fatalf("absolute expiry error = %v, want ErrAdminSessionExpired", err)
	}
	if _, err := store.GetAdminSessionByTokenHash(context.Background(), tokenHash, createdAt+11, createdAt+10); !errors.Is(err, repository.ErrAdminSessionExpired) {
		t.Fatalf("idle expiry error = %v, want ErrAdminSessionExpired", err)
	}
	if err := store.DeleteAdminSession(context.Background(), sessionID); err != nil {
		t.Fatalf("DeleteAdminSession() error = %v", err)
	}
	if err := store.DeleteAdminSession(context.Background(), sessionID); !errors.Is(err, repository.ErrAdminSessionNotFound) {
		t.Fatalf("second DeleteAdminSession() error = %v, want ErrAdminSessionNotFound", err)
	}
}

func TestAdminSessionCleanupIsBoundedAndIndexed(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateFirstAdmin(context.Background(), "admin", "password"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}
	admin, err := store.VerifyAdminCredentials(context.Background(), "admin", "password")
	if err != nil {
		t.Fatalf("VerifyAdminCredentials() error = %v", err)
	}
	const now int64 = 1_800_001_000
	for index, times := range []struct {
		created int64
		seen    int64
		expires int64
	}{
		{created: now - 900, seen: now - 900, expires: now - 1},
		{created: now - 800, seen: now - 800, expires: now + 1000},
		{created: now - 10, seen: now - 10, expires: now + 1000},
	} {
		id, idErr := identity.NewAdminSessionID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		tokenHash := sha256.Sum256([]byte{byte(index + 1)})
		var csrf [32]byte
		csrf[0] = byte(index + 1)
		if err := store.CreateAdminSession(context.Background(), repository.AdminSession{
			ID: id, UserID: admin.ID, TokenHash: tokenHash, CSRFToken: csrf,
			CreatedAt: times.created, LastSeenAt: times.seen, ExpiresAt: times.expires,
		}, times.created); err != nil {
			t.Fatalf("CreateAdminSession(%d) error = %v", index, err)
		}
	}
	deleted, err := store.DeleteExpiredAdminSessions(context.Background(), now, now-300, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpiredAdminSessions(first) = %d, %v; want 1, nil", deleted, err)
	}
	deleted, err = store.DeleteExpiredAdminSessions(context.Background(), now, now-300, 10)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpiredAdminSessions(second) = %d, %v; want 1, nil", deleted, err)
	}
	var remaining int64
	if err := store.database.Model(&adminSessionRecord{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining sessions = %d, want 1", remaining)
	}
	for _, indexName := range []string{"admin_sessions_user", "admin_sessions_expiration", "admin_sessions_idle_expiration"} {
		var count int64
		if err := store.database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", indexName).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("index %q count = %d, want 1", indexName, count)
		}
	}
	if err := store.database.Where(AdminUserColumns.ID+" = ?", admin.ID).Delete(&AdminUser{}).Error; err != nil {
		t.Fatalf("delete admin for cascade error = %v", err)
	}
	if err := store.database.Model(&adminSessionRecord{}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("sessions after admin cascade = %d, want 0", remaining)
	}
}

func TestAdminSessionSchemaRejectsInvalidSecretsAndForeignKey(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateFirstAdmin(context.Background(), "admin", "password"); err != nil {
		t.Fatal(err)
	}
	admin, err := store.VerifyAdminCredentials(context.Background(), "admin", "password")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		sessionID string
		userID    string
		tokenHash []byte
		csrfToken []byte
	}{
		{name: "非法 Session ID", sessionID: "invalid", userID: admin.ID, tokenHash: make([]byte, 32), csrfToken: make([]byte, 32)},
		{name: "短 Token Hash", sessionID: "ads_01J00000000000000000000001", userID: admin.ID, tokenHash: make([]byte, 31), csrfToken: make([]byte, 32)},
		{name: "短 CSRF Token", sessionID: "ads_01J00000000000000000000002", userID: admin.ID, tokenHash: make([]byte, 32), csrfToken: make([]byte, 31)},
		{name: "不存在的用户", sessionID: "ads_01J00000000000000000000003", userID: "adm_01J00000000000000000000001", tokenHash: make([]byte, 32), csrfToken: make([]byte, 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(
				"INSERT INTO admin_sessions(id, user_id, token_hash, csrf_token, expires_at, created_at, last_seen_at) VALUES (?, ?, ?, ?, 2, 1, 1)",
				test.sessionID, test.userID, test.tokenHash, test.csrfToken,
			).Error; err == nil {
				t.Fatal("invalid admin session insert error = nil")
			}
		})
	}
}

func TestAdminSessionMigrationUpgradesLegacyIDsAndRejectsCorruption(t *testing.T) {
	t.Run("UUID 与合法 AdminID", func(t *testing.T) {
		database := openUnmigratedDatabase(t)
		if err := runMigrations(context.Background(), database, productionMigrations[:8], testNow); err != nil {
			t.Fatalf("run v8 migrations error = %v", err)
		}
		legacyID := uuid.NewString()
		validID := "adm_01J00000000000000000000000"
		for index, admin := range []AdminUser{
			{ID: legacyID, Username: "legacy", PasswordHash: "hash", CreatedAt: 1, UpdatedAt: 2},
			{ID: validID, Username: "current", PasswordHash: "hash", CreatedAt: 2, UpdatedAt: 3},
		} {
			if err := database.Create(&admin).Error; err != nil {
				t.Fatalf("seed admin %d error = %v", index, err)
			}
		}
		if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
			t.Fatalf("upgrade to v9 error = %v", err)
		}
		var admins []AdminUser
		if err := database.Order(AdminUserColumns.Username).Find(&admins).Error; err != nil {
			t.Fatal(err)
		}
		if len(admins) != 2 || admins[0].ID != validID || !identity.ValidAdminID(admins[1].ID) || admins[1].ID == legacyID {
			t.Fatalf("migrated admins = %#v", admins)
		}
	})

	t.Run("损坏 ID 原子回滚", func(t *testing.T) {
		database := openUnmigratedDatabase(t)
		if err := runMigrations(context.Background(), database, productionMigrations[:8], testNow); err != nil {
			t.Fatal(err)
		}
		if err := database.Create(&AdminUser{ID: "corrupt", Username: "admin", PasswordHash: "hash", CreatedAt: 1, UpdatedAt: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := runMigrations(context.Background(), database, productionMigrations, testNow); err == nil {
			t.Fatal("upgrade with corrupt admin ID error = nil")
		}
		var tableCount, versionCount int64
		if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", AdminSessionTable).Scan(&tableCount).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
			t.Fatal(err)
		}
		var preserved AdminUser
		if err := database.Where(AdminUserColumns.Username+" = ?", "admin").Take(&preserved).Error; err != nil {
			t.Fatal(err)
		}
		if tableCount != 0 || versionCount != 8 || preserved.ID != "corrupt" {
			t.Fatalf("failed v9 state = table:%d versions:%d admin:%q", tableCount, versionCount, preserved.ID)
		}
	})

	t.Run("后续 DDL 失败回滚 UUID 更新", func(t *testing.T) {
		database := openUnmigratedDatabase(t)
		if err := runMigrations(context.Background(), database, productionMigrations[:8], testNow); err != nil {
			t.Fatal(err)
		}
		legacyID := uuid.NewString()
		if err := database.Create(&AdminUser{ID: legacyID, Username: "admin", PasswordHash: "hash", CreatedAt: 1, UpdatedAt: 1}).Error; err != nil {
			t.Fatal(err)
		}
		failed := append([]migration{}, productionMigrations[:8]...)
		failed = append(failed, migration{
			version: 9,
			prepare: func(ctx context.Context, transaction *gorm.DB) error {
				return migrateLegacyAdminIDs(ctx, transaction, identity.NewAdminID)
			},
			statements: []string{migrations.AdminSessions, "THIS IS NOT VALID SQL"},
		})
		if err := runMigrations(context.Background(), database, failed, testNow); err == nil {
			t.Fatal("interrupted v9 migration error = nil")
		}
		var preserved AdminUser
		if err := database.Where(AdminUserColumns.Username+" = ?", "admin").Take(&preserved).Error; err != nil {
			t.Fatal(err)
		}
		var tableCount, versionCount int64
		if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", AdminSessionTable).Scan(&tableCount).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
			t.Fatal(err)
		}
		if preserved.ID != legacyID || tableCount != 0 || versionCount != 8 {
			t.Fatalf("interrupted v9 state = admin:%q table:%d versions:%d", preserved.ID, tableCount, versionCount)
		}
	})
}

func TestVerifyPasswordRejectsUnsupportedParametersBeforeArgon2(t *testing.T) {
	valid, err := hashPassword("password")
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{
		strings.Replace(valid, "m=65536", "m=131072", 1),
		strings.Replace(valid, "t=3", "t=4", 1),
		strings.Replace(valid, ",p=", ",x=1,p=", 1),
		strings.Replace(valid, ",p=", ",m=65536,p=", 1),
		strings.Replace(valid, ",p=", ",p=5", 1),
		valid[:strings.LastIndex(valid, "$")+1] + "AA",
	}
	for index, encoded := range tests {
		if _, err := verifyPassword(encoded, "password"); err == nil {
			t.Fatalf("verifyPassword(unsupported case %d) error = nil", index)
		}
	}
}
