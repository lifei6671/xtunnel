package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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
