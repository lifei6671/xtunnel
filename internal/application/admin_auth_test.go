package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestAdminAuthenticationServiceLogin(t *testing.T) {
	now := time.Date(2026, time.August, 29, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		hasAdmin  bool
		verifyErr error
		wantErr   error
	}{
		{name: "setup required", hasAdmin: false, wantErr: ErrAdminSetupRequired},
		{name: "invalid credentials", hasAdmin: true, verifyErr: repository.ErrInvalidAdminCredentials, wantErr: ErrAdminAuthenticationFailed},
		{name: "success", hasAdmin: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeAdminAuthStore{
				hasAdmin:  test.hasAdmin,
				admin:     repository.AdminUser{ID: "adm_01J00000000000000000000000", Username: "admin"},
				verifyErr: test.verifyErr,
			}
			service := &AdminAuthenticationService{
				store:        store,
				now:          func() time.Time { return now },
				random:       bytes.NewReader(append(bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)...)),
				newSessionID: func() (string, error) { return "ads_01J00000000000000000000000", nil },
			}
			result, err := service.Login(context.Background(), "admin", "password")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Login() error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if store.created != (repository.AdminSession{}) {
					t.Fatal("Login() persisted a session after failure")
				}
				return
			}

			if result.Admin != store.admin || result.ExpiresAt != now.Add(12*time.Hour) {
				t.Fatalf("Login() result = %+v", result)
			}
			if len(result.SessionToken) != 43 || len(result.CSRFToken) != 43 || result.SessionToken == result.CSRFToken {
				t.Fatalf("Login() tokens have invalid shape")
			}
			rawSession, _ := base64.RawURLEncoding.DecodeString(result.SessionToken)
			if store.created.TokenHash != sha256.Sum256(rawSession) || store.created.CSRFToken != [32]byte(bytes.Repeat([]byte{0x22}, 32)) {
				t.Fatal("Login() did not persist the expected token hash and independent CSRF")
			}
			if store.created.UserID != store.admin.ID || store.created.ID != result.SessionID || store.loginAt != now.Unix() {
				t.Fatalf("CreateAdminSession() input = %+v, loginAt=%d", store.created, store.loginAt)
			}
			if store.cleanupNow != now.Unix() || store.cleanupIdleCutoff != now.Add(-30*time.Minute).Unix() || store.cleanupLimit != adminSessionCleanupSize {
				t.Fatalf("DeleteExpiredAdminSessions() = now %d, idle %d, limit %d", store.cleanupNow, store.cleanupIdleCutoff, store.cleanupLimit)
			}
		})
	}
}

func TestAdminAuthenticationServiceAuthenticateAndLogout(t *testing.T) {
	now := time.Date(2026, time.August, 29, 8, 30, 0, 0, time.UTC)
	rawToken := bytes.Repeat([]byte{0x44}, 32)
	token := base64.RawURLEncoding.EncodeToString(rawToken)
	store := &fakeAdminAuthStore{
		hasAdmin: true,
		session: repository.AdminSession{
			ID:         "ads_01J00000000000000000000000",
			UserID:     "adm_01J00000000000000000000000",
			TokenHash:  sha256.Sum256(rawToken),
			CSRFToken:  [32]byte(bytes.Repeat([]byte{0x55}, 32)),
			ExpiresAt:  now.Add(10 * time.Hour).Unix(),
			CreatedAt:  now.Add(-2 * time.Hour).Unix(),
			LastSeenAt: now.Add(-2 * time.Minute).Unix(),
			Admin:      repository.AdminUser{ID: "adm_01J00000000000000000000000", Username: "admin"},
		},
	}
	service := &AdminAuthenticationService{store: store, now: func() time.Time { return now }}

	result, err := service.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if result.SessionID != store.session.ID || result.Admin != store.session.Admin || result.ExpiresAt.Unix() != store.session.ExpiresAt {
		t.Fatalf("Authenticate() result = %+v", result)
	}
	if result.CSRFToken != base64.RawURLEncoding.EncodeToString(store.session.CSRFToken[:]) {
		t.Fatal("Authenticate() did not return the persisted CSRF token")
	}
	if store.touchedID != store.session.ID || store.touchedAt != now.Unix() {
		t.Fatalf("TouchAdminSession() = %q at %d", store.touchedID, store.touchedAt)
	}
	store.touchErr = repository.ErrAdminSessionExpired
	if _, err := service.Authenticate(context.Background(), token); !errors.Is(err, ErrAdminSessionExpired) {
		t.Fatalf("Authenticate(after touch expiry race) error = %v, want ErrAdminSessionExpired", err)
	}
	store.touchErr = nil

	if err := service.Logout(context.Background(), result.SessionID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if store.deletedID != result.SessionID {
		t.Fatalf("DeleteAdminSession() id = %q", store.deletedID)
	}
}

func TestAdminAuthenticationServiceRejectsInvalidOrExpiredSession(t *testing.T) {
	service := &AdminAuthenticationService{
		store: &fakeAdminAuthStore{getErr: repository.ErrAdminSessionExpired},
		now:   time.Now,
	}
	for _, token := range []string{"", "not-base64", base64.RawURLEncoding.EncodeToString(make([]byte, 31)), base64.RawURLEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := service.Authenticate(context.Background(), token); !errors.Is(err, ErrAdminSessionExpired) {
			t.Fatalf("Authenticate(%q) error = %v, want ErrAdminSessionExpired", token, err)
		}
	}
}

type fakeAdminAuthStore struct {
	hasAdmin          bool
	admin             repository.AdminUser
	verifyErr         error
	created           repository.AdminSession
	loginAt           int64
	session           repository.AdminSession
	getErr            error
	touchedID         string
	touchedAt         int64
	touchErr          error
	deletedID         string
	cleanupNow        int64
	cleanupIdleCutoff int64
	cleanupLimit      int
}

func (store *fakeAdminAuthStore) HasAdmin(context.Context) (bool, error) {
	return store.hasAdmin, nil
}

func (store *fakeAdminAuthStore) VerifyAdminCredentials(context.Context, string, string) (repository.AdminUser, error) {
	return store.admin, store.verifyErr
}

func (store *fakeAdminAuthStore) CreateAdminSession(_ context.Context, session repository.AdminSession, loginAt int64) error {
	store.created = session
	store.loginAt = loginAt
	return nil
}

func (store *fakeAdminAuthStore) GetAdminSessionByTokenHash(context.Context, [32]byte, int64, int64) (repository.AdminSession, error) {
	return store.session, store.getErr
}

func (store *fakeAdminAuthStore) TouchAdminSession(_ context.Context, id string, seenAt int64) error {
	store.touchedID = id
	store.touchedAt = seenAt
	return store.touchErr
}

func (store *fakeAdminAuthStore) DeleteAdminSession(_ context.Context, id string) error {
	store.deletedID = id
	return nil
}

func (store *fakeAdminAuthStore) DeleteExpiredAdminSessions(_ context.Context, now, idleCutoff int64, limit int) (int64, error) {
	store.cleanupNow = now
	store.cleanupIdleCutoff = idleCutoff
	store.cleanupLimit = limit
	return 0, nil
}
