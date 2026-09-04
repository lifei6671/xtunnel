package managementapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestAdminLoginSessionCSRFE2E(t *testing.T) {
	for _, localHTTP := range []bool{false, true} {
		t.Run(fmt.Sprintf("local_http=%t", localHTTP), func(t *testing.T) {
			store, err := sqlite.Open(context.Background(), t.TempDir())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Errorf("Store.Close() error = %v", err)
				}
			})

			server := httptest.NewUnstartedServer(nil)
			publicURL := "https://" + server.Listener.Addr().String()
			management := serverconfig.Management{PublicURL: publicURL}
			if localHTTP {
				publicURL = "http://" + server.Listener.Addr().String()
				management = serverconfig.Management{Listen: server.Listener.Addr().String()}
			}
			handler, err := NewHandler(HandlerOptions{
				Management: management,
				Store:      store,
				Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
			})
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			server.Config.Handler = handler
			if localHTTP {
				server.Start()
			} else {
				server.StartTLS()
			}
			t.Cleanup(server.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New() error = %v", err)
			}
			client := server.Client()
			client.Jar = jar
			page := doRequest(t, client, http.MethodGet, server.URL+"/", "", nil)
			if page.StatusCode != http.StatusOK || !strings.HasPrefix(page.Header.Get("Content-Type"), "text/html") {
				page.Body.Close()
				t.Fatalf("Web page status = %d", page.StatusCode)
			}
			page.Body.Close()

			response := postLogin(t, client, server.URL, publicURL, "admin", "correct horse battery staple")
			assertAPIError(t, response, http.StatusConflict, APIErrorCodeSETUPREQUIRED)
			if err := store.CreateFirstAdmin(context.Background(), "admin", "correct horse battery staple"); err != nil {
				t.Fatalf("CreateFirstAdmin() error = %v", err)
			}

			response = postLogin(t, client, server.URL, "https://attacker.example", "admin", "correct horse battery staple")
			assertAPIError(t, response, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED)
			response = postLogin(t, client, server.URL, publicURL, "admin", "incorrect password")
			assertAPIError(t, response, http.StatusUnauthorized, APIErrorCodeAUTHENTICATIONFAILED)

			response = postLogin(t, client, server.URL, publicURL, "admin", "correct horse battery staple")
			if response.StatusCode != http.StatusOK {
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("login status = %d, body = %s", response.StatusCode, body)
			}
			var session AuthSession
			if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
				response.Body.Close()
				t.Fatalf("decode login response: %v", err)
			}
			response.Body.Close()
			if session.Admin.Username != "admin" || !strings.HasPrefix(session.Admin.Id, "adm_") || !validCSRF(session.CsrfToken, session.CsrfToken) {
				t.Fatalf("login session = %#v", session)
			}
			cookies := response.Cookies()
			if len(cookies) != 1 || cookies[0].Name != adminSessionCookieName || cookies[0].Secure == localHTTP || !cookies[0].HttpOnly ||
				cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/api/v1" || cookies[0].Domain != "" {
				t.Fatalf("login cookies = %#v", cookies)
			}
			if got := response.Header.Get("Cache-Control"); got != "no-store" {
				t.Fatalf("login Cache-Control = %q", got)
			}

			response = doRequest(t, client, http.MethodGet, server.URL+"/api/v1/auth/me", "", nil)
			if response.StatusCode != http.StatusOK {
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("auth/me status = %d, body = %s", response.StatusCode, body)
			}
			var restored AuthSession
			if err := json.NewDecoder(response.Body).Decode(&restored); err != nil {
				response.Body.Close()
				t.Fatalf("decode auth/me response: %v", err)
			}
			response.Body.Close()
			if restored.CsrfToken != session.CsrfToken || restored.Admin != session.Admin {
				t.Fatalf("auth/me session = %#v, want persisted %#v", restored, session)
			}

			response = doRequest(t, client, http.MethodPost, server.URL+"/api/v1/auth/logout", publicURL, map[string]string{
				"X-XTunnel-CSRF": strings.Repeat("A", 43),
			})
			assertAPIError(t, response, http.StatusForbidden, APIErrorCodeCSRFINVALID)
			response = doRequest(t, client, http.MethodPost, server.URL+"/api/v1/auth/logout", publicURL, map[string]string{
				"X-XTunnel-CSRF": session.CsrfToken,
			})
			if response.StatusCode != http.StatusNoContent {
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("logout status = %d, body = %s", response.StatusCode, body)
			}
			response.Body.Close()
			response = doRequest(t, client, http.MethodGet, server.URL+"/api/v1/auth/me", "", nil)
			assertAPIError(t, response, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)
		})
	}
}

func TestLoginRejectsUnknownFieldsAndDuplicateSessionCookie(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: "https://admin.example"},
		Store:      store,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "https://admin.example/api/v1/auth/login", bytes.NewBufferString(
		`{"username":"admin","password":"secret","extra":true}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://admin.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field login status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "https://admin.example/api/v1/auth/login", bytes.NewBufferString(
		`{"username":"admin","password":"secret"}`,
	))
	request.Header["Content-Type"] = []string{"application/json", "application/json; charset=utf-8"}
	request.Header.Set("Origin", "https://admin.example")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate-content-type login status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "https://admin.example/api/v1/auth/me", nil)
	request.Header.Add("Cookie", adminSessionCookieName+"=first; "+adminSessionCookieName+"=second")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate-cookie auth/me status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGeneratedAuthRouterOwnsMethodAndNotFoundResponses(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: "https://admin.example"},
		Store:      store,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	for _, test := range []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/api/v1/auth/login", status: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/v1/auth/unknown", status: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/v1", status: http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, "https://admin.example"+test.path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != test.status {
			t.Errorf("%s %s status = %d, want %d; body = %s", test.method, test.path, recorder.Code, test.status, recorder.Body.String())
		}
		if strings.Contains(recorder.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s %s fell through to SPA", test.method, test.path)
		}
	}
}

func TestLoginPasswordVerificationConcurrencyIsBounded(t *testing.T) {
	store := &blockingAdminAuthStore{
		entered: make(chan struct{}, loginPasswordVerificationConcurrency),
		release: make(chan struct{}),
	}
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: "https://admin.example"},
		Store:      store,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	const requests = loginPasswordVerificationConcurrency + 8
	start := make(chan struct{})
	results := make(chan int, requests)
	for range requests {
		go func() {
			<-start
			request := httptest.NewRequest(http.MethodPost, "https://admin.example/api/v1/auth/login", bytes.NewBufferString(
				`{"username":"admin","password":"secret"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "https://admin.example")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			results <- recorder.Code
		}()
	}
	close(start)
	for range loginPasswordVerificationConcurrency {
		select {
		case <-store.entered:
		case <-time.After(time.Second):
			t.Fatal("password verification did not fill all slots")
		}
	}
	for range requests - loginPasswordVerificationConcurrency {
		select {
		case status := <-results:
			if status != http.StatusTooManyRequests {
				t.Fatalf("overflow login status = %d, want 429", status)
			}
		case <-time.After(time.Second):
			t.Fatal("overflow login did not fail without waiting")
		}
	}
	close(store.release)
	for range loginPasswordVerificationConcurrency {
		select {
		case status := <-results:
			if status != http.StatusUnauthorized {
				t.Fatalf("verified login status = %d, want 401", status)
			}
		case <-time.After(time.Second):
			t.Fatal("verified login did not finish")
		}
	}
	if got := store.maxActive.Load(); got != loginPasswordVerificationConcurrency {
		t.Fatalf("max concurrent password verifications = %d, want %d", got, loginPasswordVerificationConcurrency)
	}
}

type blockingAdminAuthStore struct {
	entered   chan struct{}
	release   chan struct{}
	active    atomic.Int64
	maxActive atomic.Int64
}

func (*blockingAdminAuthStore) HasAdmin(context.Context) (bool, error) {
	return true, nil
}

func (store *blockingAdminAuthStore) VerifyAdminCredentials(context.Context, string, string) (repository.AdminUser, error) {
	active := store.active.Add(1)
	defer store.active.Add(-1)
	for {
		maximum := store.maxActive.Load()
		if active <= maximum || store.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	store.entered <- struct{}{}
	<-store.release
	return repository.AdminUser{}, repository.ErrInvalidAdminCredentials
}

func (*blockingAdminAuthStore) CreateAdminSession(context.Context, repository.AdminSession, int64) error {
	panic("unexpected CreateAdminSession")
}

func (*blockingAdminAuthStore) GetAdminSessionByTokenHash(context.Context, [32]byte, int64, int64) (repository.AdminSession, error) {
	panic("unexpected GetAdminSessionByTokenHash")
}

func (*blockingAdminAuthStore) TouchAdminSession(context.Context, string, int64) error {
	panic("unexpected TouchAdminSession")
}

func (*blockingAdminAuthStore) DeleteAdminSession(context.Context, string) error {
	panic("unexpected DeleteAdminSession")
}

func (*blockingAdminAuthStore) DeleteExpiredAdminSessions(context.Context, int64, int64, int) (int64, error) {
	panic("unexpected DeleteExpiredAdminSessions")
}

func postLogin(t *testing.T, client *http.Client, baseURL, origin, username, password string) *http.Response {
	t.Helper()
	body, err := json.Marshal(LoginRequest{Username: username, Password: &password})
	if err != nil {
		t.Fatalf("json.Marshal(login) error = %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest(login) error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(login) error = %v", err)
	}
	return response
}

func doRequest(t *testing.T, client *http.Client, method, target, origin string, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("http.NewRequest(%s) error = %v", method, err)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(%s) error = %v", method, err)
	}
	return response
}

func assertAPIError(t *testing.T, response *http.Response, status int, code APIErrorCode) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != status {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("response status = %d, want %d; body = %s", response.StatusCode, status, body)
	}
	var body ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode API error: %v", err)
	}
	if body.Error.Code != code || body.Error.RequestId == "" {
		t.Fatalf("API error = %#v, want code %s with request_id", body.Error, code)
	}
}
