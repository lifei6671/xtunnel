package managementapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

type tunnelAPIHarness struct {
	server    *httptest.Server
	client    *http.Client
	publicURL string
	csrf      string
	runtime   *tunnelRuntimeFake
	store     *sqlite.Store
}

type tunnelRuntimeFake struct {
	mu         sync.RWMutex
	statuses   []serverruntime.SessionStatusSnapshot
	connectors []serverruntime.ConnectorSnapshot
	revokeErr  error
	deleteErr  error
}

func (runtime *tunnelRuntimeFake) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return append([]serverruntime.SessionStatusSnapshot(nil), runtime.statuses...)
}

func (runtime *tunnelRuntimeFake) ConnectorSnapshots() []serverruntime.ConnectorSnapshot {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return append([]serverruntime.ConnectorSnapshot(nil), runtime.connectors...)
}

func (runtime *tunnelRuntimeFake) RevokeTunnel(string) error {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.revokeErr
}

func (runtime *tunnelRuntimeFake) DeleteTunnel(string) error {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.deleteErr
}

func TestTunnelCredentialWorkflowE2E(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "edge tunnel")
	if created.Tunnel.Status != TunnelStatusPENDING || created.Tunnel.Version != 1 || created.Credential.TokenVersion != 1 {
		t.Fatalf("create projection status/version is incorrect")
	}
	if len(created.Credential.DeploymentCommands) != 4 {
		t.Fatalf("deployment command count = %d, want 4", len(created.Credential.DeploymentCommands))
	}
	for _, command := range created.Credential.DeploymentCommands {
		if !strings.Contains(command.Command, created.Credential.ConnectionToken) {
			t.Fatal("deployment command does not contain the issued token")
		}
	}

	reveal := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id+"/token", nil, "", false)
	assertSecretHeaders(t, reveal)
	var revealed ConnectionCredential
	decodeSuccess(t, reveal, &revealed)
	if revealed.ConnectionToken != created.Credential.ConnectionToken || revealed.TokenId != created.Credential.TokenId || revealed.TokenVersion != 1 {
		t.Fatal("Reveal did not return the stable current credential")
	}

	patchBody := map[string]string{"name": "renamed tunnel"}
	patchedResponse := doTunnelRequest(t, harness, http.MethodPatch, "/api/v1/tunnels/"+created.Tunnel.Id, patchBody, `"1"`, true)
	if patchedResponse.StatusCode != http.StatusOK || patchedResponse.Header.Get("ETag") != `"2"` {
		patchedResponse.Body.Close()
		t.Fatalf("PATCH status/ETag = %d/%q, want 200/\"2\"", patchedResponse.StatusCode, patchedResponse.Header.Get("ETag"))
	}
	var patched Tunnel
	decodeSuccess(t, patchedResponse, &patched)
	if patched.Name != "renamed tunnel" || patched.Version != 2 {
		t.Fatalf("patched tunnel name/version are incorrect")
	}

	rotatedResponse := doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/token/rotate", nil, `"2"`, true)
	assertSecretHeaders(t, rotatedResponse)
	if rotatedResponse.StatusCode != http.StatusOK || rotatedResponse.Header.Get("ETag") != `"3"` {
		rotatedResponse.Body.Close()
		t.Fatalf("rotate status/ETag = %d/%q, want 200/\"3\"", rotatedResponse.StatusCode, rotatedResponse.Header.Get("ETag"))
	}
	var rotated ConnectionCredential
	decodeSuccess(t, rotatedResponse, &rotated)
	if rotated.TokenVersion != 2 || rotated.ConnectionToken == created.Credential.ConnectionToken {
		t.Fatal("Rotate did not issue a distinct second credential")
	}

	revokedResponse := doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/token/revoke", nil, `"3"`, true)
	if revokedResponse.StatusCode != http.StatusOK || revokedResponse.Header.Get("ETag") != `"4"` {
		revokedResponse.Body.Close()
		t.Fatalf("token revoke status/ETag = %d/%q, want 200/\"4\"", revokedResponse.StatusCode, revokedResponse.Header.Get("ETag"))
	}
	bodyBytes, err := io.ReadAll(revokedResponse.Body)
	revokedResponse.Body.Close()
	if err != nil {
		t.Fatalf("read token revoke response: %v", err)
	}
	if bytes.Contains(bodyBytes, []byte("connection_token")) || bytes.Contains(bodyBytes, []byte("deployment_commands")) ||
		bytes.Contains(bodyBytes, []byte(rotated.ConnectionToken)) {
		t.Fatal("token revoke metadata leaked secret material")
	}
	var metadata ConnectionCredentialMetadata
	if err := json.Unmarshal(bodyBytes, &metadata); err != nil || metadata.Status != ConnectionCredentialMetadataStatusREVOKED {
		t.Fatal("token revoke did not return REVOKED metadata")
	}

	reveal = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id+"/token", nil, "", false)
	assertAPIError(t, reveal, http.StatusConflict, APIErrorCodeCONNECTIONTOKENUNAVAILABLE)

	revokeTunnel := doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/revoke", nil, `"4"`, true)
	if revokeTunnel.StatusCode != http.StatusOK || revokeTunnel.Header.Get("ETag") != `"5"` {
		revokeTunnel.Body.Close()
		t.Fatalf("tunnel revoke status/ETag = %d/%q, want 200/\"5\"", revokeTunnel.StatusCode, revokeTunnel.Header.Get("ETag"))
	}
	var revokedTunnel Tunnel
	decodeSuccess(t, revokeTunnel, &revokedTunnel)
	if revokedTunnel.Status != TunnelStatusREVOKED || !revokedTunnel.RevokedAt.IsSpecified() {
		t.Fatal("tunnel revoke did not publish the durable tombstone")
	}
}

func TestTunnelCRUDSecurityAndConnectorRuntime(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "connector tunnel")

	unauthenticated, err := http.NewRequest(http.MethodGet, harness.server.URL+"/api/v1/tunnels", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	unauthenticatedClient := &http.Client{Transport: harness.server.Client().Transport}
	response, err := unauthenticatedClient.Do(unauthenticated)
	if err != nil {
		t.Fatalf("unauthenticated request error = %v", err)
	}
	assertAPIError(t, response, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)

	response = doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/token/rotate", nil, `"1"`, false)
	assertAPIError(t, response, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED)
	response = doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/token/rotate", nil, `"1"`, true)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("authorized rotate status = %d, want 200", response.StatusCode)
	}
	response.Body.Close()

	now := time.Now().UTC().Truncate(time.Second)
	harness.runtime.mu.Lock()
	harness.runtime.statuses = []serverruntime.SessionStatusSnapshot{{
		Session:           serverruntime.Session{TunnelID: created.Tunnel.Id, ConnectorID: "con_01J00000000000000000000000", SessionID: "sess_01J00000000000000000000000", Generation: 1},
		ConnectorMetadata: serverruntime.ConnectorMetadata{Hostname: "edge-a", OS: "linux", Arch: "amd64", Version: "v0.1.0"},
		ConnectedAt:       now.Add(-time.Minute), LastHeartbeatAt: now,
		CurrentControlSession: true, HeartbeatFresh: true,
		Config:   serverruntime.SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 3},
		WorkPool: serverruntime.ConnectorWorkPoolSnapshot{Idle: 2, Active: 1},
	}}
	harness.runtime.connectors = []serverruntime.ConnectorSnapshot{{
		Session:         serverruntime.Session{TunnelID: created.Tunnel.Id, ConnectorID: "con_01J00000000000000000000000", SessionID: "sess_01J00000000000000000000000", Generation: 1},
		LastHeartbeatAt: now, ActiveWork: 1,
	}}
	harness.runtime.mu.Unlock()

	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id+"/connectors", nil, "", false)
	var connectors ConnectorList
	decodeSuccess(t, response, &connectors)
	if len(connectors.Items) != 1 || connectors.Items[0].Status != ConnectorStatusONLINE ||
		connectors.Items[0].IdleWorkConnections != 2 || connectors.Items[0].ObservedRevision != 3 {
		t.Fatalf("connector projection = %#v", connectors.Items)
	}

	response = doTunnelRequest(t, harness, http.MethodDelete, "/api/v1/tunnels/"+created.Tunnel.Id, nil, `"2"`, true)
	if response.StatusCode != http.StatusNoContent {
		response.Body.Close()
		t.Fatalf("DELETE status = %d, want 204", response.StatusCode)
	}
	response.Body.Close()
	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id, nil, "", false)
	assertAPIError(t, response, http.StatusNotFound, APIErrorCodeRESOURCENOTFOUND)
}

func TestTunnelAndConnectorOpaquePaginationOverTLS(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	firstTunnel := createTunnelForTest(t, harness, "page tunnel a")
	createTunnelForTest(t, harness, "page tunnel b")
	createTunnelForTest(t, harness, "page tunnel c")

	response := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_size=1", nil, "", false)
	var firstPage TunnelList
	decodeSuccess(t, response, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.NextPageToken == nil {
		t.Fatalf("first Tunnel page = %#v", firstPage)
	}
	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_size=1&page_token="+*firstPage.NextPageToken, nil, "", false)
	var secondPage TunnelList
	decodeSuccess(t, response, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].Id <= firstPage.Items[0].Id || secondPage.NextPageToken == nil {
		t.Fatalf("second Tunnel page = %#v after %#v", secondPage, firstPage)
	}
	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_size=1&page_token="+*secondPage.NextPageToken, nil, "", false)
	terminalTunnelBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read terminal Tunnel page: %v", err)
	}
	var terminalTunnelPage TunnelList
	if err := json.Unmarshal(terminalTunnelBody, &terminalTunnelPage); err != nil {
		t.Fatalf("decode terminal Tunnel page: %v", err)
	}
	if len(terminalTunnelPage.Items) != 1 || terminalTunnelPage.NextPageToken != nil ||
		bytes.Contains(terminalTunnelBody, []byte(`"next_page_token"`)) {
		t.Fatalf("terminal Tunnel page must omit next_page_token: %s", terminalTunnelBody)
	}

	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_size=1&page_token="+*firstPage.NextPageToken+"&status=PENDING", nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_token=", nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
	response = doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels?page_size=201", nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)

	now := time.Now().UTC().Truncate(time.Second)
	harness.runtime.mu.Lock()
	for index, connectorID := range []string{"con_01J00000000000000000000000", "con_01J00000000000000000000001"} {
		sessionID := []string{"sess_01J00000000000000000000000", "sess_01J00000000000000000000001"}[index]
		harness.runtime.statuses = append(harness.runtime.statuses, serverruntime.SessionStatusSnapshot{
			Session: serverruntime.Session{
				TunnelID: firstTunnel.Tunnel.Id, ConnectorID: connectorID,
				SessionID: sessionID, Generation: 1,
			},
			ConnectorMetadata: serverruntime.ConnectorMetadata{Hostname: "edge", OS: "linux", Arch: "amd64", Version: "v0.1.0"},
			ConnectedAt:       now.Add(-time.Minute), LastHeartbeatAt: now,
			CurrentControlSession: true, HeartbeatFresh: true,
			Config: serverruntime.SessionEligibility{ConfigReady: true, HasObserved: true, ObservedRevision: 1},
		})
	}
	harness.runtime.mu.Unlock()

	connectorPath := "/api/v1/tunnels/" + firstTunnel.Tunnel.Id + "/connectors?page_size=1"
	response = doTunnelRequest(t, harness, http.MethodGet, connectorPath, nil, "", false)
	var connectorPage ConnectorList
	decodeSuccess(t, response, &connectorPage)
	if len(connectorPage.Items) != 1 || connectorPage.NextPageToken == nil {
		t.Fatalf("first Connector page = %#v", connectorPage)
	}
	response = doTunnelRequest(t, harness, http.MethodGet, connectorPath+"&page_token="+*connectorPage.NextPageToken, nil, "", false)
	terminalConnectorBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read terminal Connector page: %v", err)
	}
	var nextConnectorPage ConnectorList
	if err := json.Unmarshal(terminalConnectorBody, &nextConnectorPage); err != nil {
		t.Fatalf("decode terminal Connector page: %v", err)
	}
	if len(nextConnectorPage.Items) != 1 || nextConnectorPage.Items[0].Id <= connectorPage.Items[0].Id ||
		nextConnectorPage.NextPageToken != nil || bytes.Contains(terminalConnectorBody, []byte(`"next_page_token"`)) {
		t.Fatalf("second Connector page = %#v after %#v", nextConnectorPage, connectorPage)
	}
	response = doTunnelRequest(t, harness, http.MethodGet, connectorPath+"&page_token="+*firstPage.NextPageToken, nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
}

func TestDeleteTunnelRejectsServiceReferences(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "referenced tunnel")
	serviceRecord := repository.Service{
		ID: "svc_01J00000000000000000000001", TunnelID: created.Tunnel.Id, Name: "web",
		RequiredRevision: 1, OriginScheme: repository.OriginSchemeHTTP,
		OriginHost: "127.0.0.1", OriginPort: 8080, ConnectTimeoutMS: 5_000,
		ProxyOptions: (repository.ServiceProxyOptions{}).WithDefaults(), Enabled: true,
		Version: 1, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := harness.store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Services().Create(context.Background(), serviceRecord)
	}); err != nil {
		t.Fatalf("seed referenced Service error = %v", err)
	}
	response := doTunnelRequest(t, harness, http.MethodDelete, "/api/v1/tunnels/"+created.Tunnel.Id, nil, `"1"`, true)
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("DELETE referenced tunnel status = %d, want 409", response.StatusCode)
	}
	var body ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode TUNNEL_IN_USE response: %v", err)
	}
	if body.Error.Code != APIErrorCodeTUNNELINUSE || body.Error.Details == nil {
		t.Fatalf("DELETE referenced tunnel error code/details are incomplete")
	}
	details, err := body.Error.Details.AsTunnelInUseDetails()
	if err != nil || details.ServiceCount != 1 || len(details.ReferencingServiceIds) != 1 ||
		details.ReferencingServiceIds[0] != serviceRecord.ID || details.ReferencesTruncated {
		t.Fatalf("TUNNEL_IN_USE details are incorrect")
	}
	get := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id, nil, "", false)
	if get.StatusCode != http.StatusOK {
		get.Body.Close()
		t.Fatalf("referenced Tunnel was deleted; GET status = %d", get.StatusCode)
	}
	get.Body.Close()
}

func TestTunnelMutationPreconditionsAndStrictJSON(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "precondition tunnel")
	path := "/api/v1/tunnels/" + created.Tunnel.Id

	response := doTunnelRequest(t, harness, http.MethodPatch, path, map[string]string{"name": "next"}, "", true)
	assertAPIError(t, response, http.StatusPreconditionRequired, APIErrorCodePRECONDITIONREQUIRED)
	response = doTunnelRequest(t, harness, http.MethodPatch, path, map[string]string{"name": "next"}, `W/"1"`, true)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDIFMATCH)
	response = doTunnelRequest(t, harness, http.MethodPatch, path, map[string]string{"name": "next"}, `"9"`, true)
	assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)
	response = doTunnelRequest(t, harness, http.MethodPatch, path, map[string]any{"name": nil}, `"1"`, true)
	assertAPIError(t, response, http.StatusUnprocessableEntity, APIErrorCodeVALIDATIONFAILED)
	response = doTunnelRequest(t, harness, http.MethodPatch, path, map[string]any{"name": "next", "unknown": true}, `"1"`, true)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)

	body := strings.NewReader(`{"name":"bad","unknown":true}`)
	request, err := http.NewRequest(http.MethodPost, harness.server.URL+"/api/v1/tunnels", body)
	if err != nil {
		t.Fatalf("http.NewRequest(create invalid) error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", harness.publicURL)
	request.Header.Set("X-XTunnel-CSRF", harness.csrf)
	response, err = harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(create invalid) error = %v", err)
	}
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)

	request, err = http.NewRequest(http.MethodPost, harness.server.URL+path+"/token/rotate", nil)
	if err != nil {
		t.Fatalf("http.NewRequest(rotate missing CSRF) error = %v", err)
	}
	request.Header.Set("Origin", harness.publicURL)
	request.Header.Set("If-Match", `"1"`)
	response, err = harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(rotate missing CSRF) error = %v", err)
	}
	assertAPIError(t, response, http.StatusForbidden, APIErrorCodeCSRFINVALID)
}

func TestTunnelMutationPreconditionMatrixOverTLS(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "tunnel precondition matrix")
	basePath := "/api/v1/tunnels/" + created.Tunnel.Id
	tests := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "delete", method: http.MethodDelete, path: basePath},
		{name: "rotate token", method: http.MethodPost, path: basePath + "/token/rotate"},
		{name: "revoke token", method: http.MethodPost, path: basePath + "/token/revoke"},
		{name: "revoke tunnel", method: http.MethodPost, path: basePath + "/revoke"},
	}
	for _, test := range tests {
		t.Run(test.name+" missing", func(t *testing.T) {
			response := doTunnelRequest(t, harness, test.method, test.path, test.body, "", true)
			assertAPIError(t, response, http.StatusPreconditionRequired, APIErrorCodePRECONDITIONREQUIRED)
		})
		t.Run(test.name+" stale", func(t *testing.T) {
			response := doTunnelRequest(t, harness, test.method, test.path, test.body, `"9"`, true)
			assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)
		})
	}
}

func TestTunnelPatchRejectsJSONMediaTypeOverTLS(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "tunnel media type")
	request, err := http.NewRequest(
		http.MethodPatch,
		harness.server.URL+"/api/v1/tunnels/"+created.Tunnel.Id,
		strings.NewReader(`{"name":"wrong media type"}`),
	)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", harness.publicURL)
	request.Header.Set("X-XTunnel-CSRF", harness.csrf)
	request.Header.Set("If-Match", `"1"`)
	response, err := harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
}

func TestTunnelConcurrentPatchWithSameETagCommitsOnce(t *testing.T) {
	harness := newTunnelAPIHarness(t)
	created := createTunnelForTest(t, harness, "concurrent tunnel")
	path := "/api/v1/tunnels/" + created.Tunnel.Id

	type patchResult struct {
		status int
		etag   string
		body   []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan patchResult, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"winner-a", "winner-b"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			body, err := json.Marshal(map[string]string{"name": name})
			if err != nil {
				results <- patchResult{err: err}
				return
			}
			request, err := http.NewRequest(http.MethodPatch, harness.server.URL+path, bytes.NewReader(body))
			if err != nil {
				results <- patchResult{err: err}
				return
			}
			request.Header.Set("Content-Type", "application/merge-patch+json")
			request.Header.Set("Origin", harness.publicURL)
			request.Header.Set("X-XTunnel-CSRF", harness.csrf)
			request.Header.Set("If-Match", `"1"`)
			response, err := harness.client.Do(request)
			if err != nil {
				results <- patchResult{err: err}
				return
			}
			responseBody, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			results <- patchResult{
				status: response.StatusCode, etag: response.Header.Get("ETag"), body: responseBody,
				err: errors.Join(readErr, closeErr),
			}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	var succeeded, conflicted int
	var committed Tunnel
	var committedETag string
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent PATCH error = %v", result.err)
		}
		switch result.status {
		case http.StatusOK:
			succeeded++
			committedETag = result.etag
			if err := json.Unmarshal(result.body, &committed); err != nil {
				t.Fatalf("decode committed Tunnel response: %v", err)
			}
		case http.StatusPreconditionFailed:
			conflicted++
		default:
			t.Fatalf("concurrent PATCH status = %d", result.status)
		}
	}
	if succeeded != 1 || conflicted != 1 || committedETag != `"2"` {
		t.Fatalf("concurrent PATCH results = succeeded:%d conflicted:%d, want 1/1", succeeded, conflicted)
	}

	response := doTunnelRequest(t, harness, http.MethodGet, path, nil, "", false)
	if response.Header.Get("ETag") != `"2"` {
		response.Body.Close()
		t.Fatalf("final Tunnel ETag = %q, want \"2\"", response.Header.Get("ETag"))
	}
	var tunnel Tunnel
	decodeSuccess(t, response, &tunnel)
	if committed.Version != 2 || tunnel.Version != committed.Version || tunnel.Name != committed.Name ||
		tunnel.Name != "winner-a" && tunnel.Name != "winner-b" {
		t.Fatalf("final Tunnel = version:%d name:%q", tunnel.Version, tunnel.Name)
	}
}

func TestIgnorableTunnelPostCommitCleanupDoesNotHideRuntimeConvergence(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		committed bool
		want      bool
	}{
		{name: "cleanup only after commit", err: repository.ErrPostCommitCleanup, committed: true, want: true},
		{name: "cleanup before commit result", err: repository.ErrPostCommitCleanup, committed: false},
		{
			name:      "tunnel revoke convergence joined with cleanup",
			err:       errors.Join(repository.ErrPostCommitCleanup, application.ErrTunnelRuntimeConvergence),
			committed: true,
		},
		{
			name:      "tunnel delete convergence joined with cleanup",
			err:       errors.Join(repository.ErrPostCommitCleanup, application.ErrTunnelManagementRuntimeConvergence),
			committed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ignorableTunnelPostCommitCleanup(test.err, test.committed); got != test.want {
				t.Fatalf("ignorableTunnelPostCommitCleanup() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestTunnelRuntimeConvergenceFailureReportsCommittedState(t *testing.T) {
	t.Run("revoke", func(t *testing.T) {
		harness := newTunnelAPIHarness(t)
		created := createTunnelForTest(t, harness, "revoke convergence")
		harness.runtime.mu.Lock()
		harness.runtime.revokeErr = errors.New("injected runtime failure")
		harness.runtime.mu.Unlock()

		response := doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels/"+created.Tunnel.Id+"/revoke", nil, `"1"`, true)
		assertAPIError(t, response, http.StatusConflict, APIErrorCodeRUNTIMECONVERGENCEFAILED)
		get := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id, nil, "", false)
		var tunnel Tunnel
		decodeSuccess(t, get, &tunnel)
		if tunnel.Status != TunnelStatusREVOKED {
			t.Fatal("runtime failure rolled back the durable Tunnel revoke")
		}
	})

	t.Run("delete", func(t *testing.T) {
		harness := newTunnelAPIHarness(t)
		created := createTunnelForTest(t, harness, "delete convergence")
		harness.runtime.mu.Lock()
		harness.runtime.deleteErr = errors.New("injected runtime failure")
		harness.runtime.mu.Unlock()

		response := doTunnelRequest(t, harness, http.MethodDelete, "/api/v1/tunnels/"+created.Tunnel.Id, nil, `"1"`, true)
		assertAPIError(t, response, http.StatusInternalServerError, APIErrorCodeRUNTIMECONVERGENCEFAILED)
		get := doTunnelRequest(t, harness, http.MethodGet, "/api/v1/tunnels/"+created.Tunnel.Id, nil, "", false)
		assertAPIError(t, get, http.StatusNotFound, APIErrorCodeRESOURCENOTFOUND)
	})
}

func newTunnelAPIHarness(t *testing.T) *tunnelAPIHarness {
	t.Helper()
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.CreateFirstAdmin(context.Background(), "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}
	protector, err := application.NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	tokens := application.NewConnectionTokenService(store, protector)
	runtime := &tunnelRuntimeFake{}
	endpoint := &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 443}
	trust := &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}}}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	audit := application.NewSecurityAuditWriter(store, logger)

	server := httptest.NewUnstartedServer(nil)
	publicURL := "https://" + server.Listener.Addr().String()
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: publicURL}, Store: store,
		Tunnels:         application.NewTunnelManagementService(store, tokens, runtime, endpoint, trust, 1000),
		Credentials:     application.NewCredentialLifecycleService(tokens, audit),
		TunnelLifecycle: application.NewTunnelLifecycleService(store, audit, runtime),
		Logger:          logger,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server.Config.Handler = handler
	server.StartTLS()
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	client := server.Client()
	client.Jar = jar
	login := postLogin(t, client, server.URL, publicURL, "admin", "correct horse battery staple")
	if login.StatusCode != http.StatusOK {
		login.Body.Close()
		t.Fatalf("login status = %d, want 200", login.StatusCode)
	}
	var session AuthSession
	decodeSuccess(t, login, &session)
	return &tunnelAPIHarness{
		server: server, client: client, publicURL: publicURL, csrf: session.CsrfToken,
		runtime: runtime, store: store,
	}
}

func createTunnelForTest(t *testing.T, harness *tunnelAPIHarness, name string) TunnelCredentialResponse {
	t.Helper()
	response := doTunnelRequest(t, harness, http.MethodPost, "/api/v1/tunnels", CreateTunnelRequest{Name: name}, "", true)
	assertSecretHeaders(t, response)
	if response.StatusCode != http.StatusCreated || response.Header.Get("ETag") != `"1"` || response.Header.Get("Location") == "" {
		response.Body.Close()
		t.Fatalf("create status/headers = %d/%q/%q", response.StatusCode, response.Header.Get("ETag"), response.Header.Get("Location"))
	}
	var result TunnelCredentialResponse
	decodeSuccess(t, response, &result)
	return result
}

func doTunnelRequest(t *testing.T, harness *tunnelAPIHarness, method, path string, body any, ifMatch string, mutationHeaders bool) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal(request) error = %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, harness.server.URL+path, reader)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if body != nil {
		if method == http.MethodPatch {
			request.Header.Set("Content-Type", "application/merge-patch+json")
		} else {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	if mutationHeaders {
		request.Header.Set("Origin", harness.publicURL)
		request.Header.Set("X-XTunnel-CSRF", harness.csrf)
	}
	response, err := harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do(%s) error = %v", path, err)
	}
	return response
}

func decodeSuccess(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("response status = %d, want success", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode success response: %v", err)
	}
}

func assertSecretHeaders(t *testing.T, response *http.Response) {
	t.Helper()
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
		response.Body.Close()
		t.Fatalf("secret cache headers = %q/%q", response.Header.Get("Cache-Control"), response.Header.Get("Pragma"))
	}
}
