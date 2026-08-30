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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

func TestServiceAPIHTTPExposureLifecycleOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service API")
	createBody := map[string]any{
		"tunnel_id": tunnel.Tunnel.Id,
		"name":      "web",
		"origin": map[string]any{
			"scheme": "https", "host": "origin.example.test", "port": 8443,
			"tls_verify": true, "tls_server_name": "origin.example.test",
		},
		"exposure": map[string]any{"type": "http", "hostname": "public.example.test"},
	}
	createdResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", createBody, `"1"`, true)
	if createdResponse.StatusCode != http.StatusCreated || !validServiceIfMatch(createdResponse.Header.Get("ETag")) {
		body, _ := io.ReadAll(createdResponse.Body)
		createdResponse.Body.Close()
		t.Fatalf("create Service status/ETag/body = %d/%q/%s", createdResponse.StatusCode, createdResponse.Header.Get("ETag"), body)
	}
	etag := createdResponse.Header.Get("ETag")
	var created Service
	decodeSuccess(t, createdResponse, &created)
	if created.Status != ServiceStatusTUNNELOFFLINE || created.Usage.Availability != UsageSummaryAvailabilityUNAVAILABLE ||
		!created.Usage.ConnectionsToday.IsNull() || !created.Usage.IngressBytesToday.IsNull() || !created.Usage.EgressBytesToday.IsNull() {
		t.Fatalf("created status/usage = %s/%#v", created.Status, created.Usage)
	}
	httpsOrigin, err := created.Origin.AsHTTPSOrigin()
	if err != nil || !httpsOrigin.TlsVerify || httpsOrigin.TlsServerName == nil || *httpsOrigin.TlsServerName != "origin.example.test" {
		t.Fatalf("created HTTPS origin = %#v, error = %v", httpsOrigin, err)
	}
	exposureValue, err := created.Exposure.Get()
	if err != nil {
		t.Fatalf("created Exposure.Get() error = %v", err)
	}
	httpExposure, err := exposureValue.AsHTTPExposure()
	if err != nil || httpExposure.PathPrefix != "/" || !httpExposure.PreserveHost {
		t.Fatalf("created HTTP exposure = %#v, error = %v", httpExposure, err)
	}

	unauthenticated := &http.Client{Transport: harness.server.Client().Transport}
	unauthorized, err := unauthenticated.Get(harness.server.URL + "/api/v1/services/" + created.Id)
	if err != nil {
		t.Fatalf("unauthenticated GET error = %v", err)
	}
	assertAPIError(t, unauthorized, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED)

	missingCSRF := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id, map[string]any{"name": "web v2"}, etag, false)
	assertAPIError(t, missingCSRF, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED)

	patchedResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id,
		map[string]any{"name": "web v2", "exposure": map[string]any{"path_prefix": "/api"}}, etag, true)
	var patched Service
	decodeSuccess(t, patchedResponse, &patched)
	if patched.Name != "web v2" || patchedResponse.Header.Get("ETag") == etag {
		t.Fatalf("patched Service name/ETag = %q/%q", patched.Name, patchedResponse.Header.Get("ETag"))
	}
	etag = patchedResponse.Header.Get("ETag")

	listedResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/services?tunnel_id="+tunnel.Tunnel.Id, nil, "", false)
	var listed ServiceList
	decodeSuccess(t, listedResponse, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Id != created.Id {
		t.Fatalf("listed Services = %#v", listed.Items)
	}

	disabledResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services/"+created.Id+"/disable", nil, etag, true)
	var disabled Service
	decodeSuccess(t, disabledResponse, &disabled)
	if disabled.Enabled || disabled.Status != ServiceStatusDISABLED {
		t.Fatalf("disabled Service = enabled:%t status:%s", disabled.Enabled, disabled.Status)
	}
	disabledETag := disabledResponse.Header.Get("ETag")
	if disabledETag == etag {
		t.Fatal("disable did not advance Service ETag")
	}

	enabledResponse := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services/"+created.Id+"/enable", nil, disabledETag, true)
	var enabled Service
	decodeSuccess(t, enabledResponse, &enabled)
	if !enabled.Enabled || enabled.Status != ServiceStatusTUNNELOFFLINE || enabledResponse.Header.Get("ETag") == disabledETag {
		t.Fatalf("enabled Service = enabled:%t status:%s ETag:%q", enabled.Enabled, enabled.Status, enabledResponse.Header.Get("ETag"))
	}

	deleted := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodDelete, "/api/v1/services/"+created.Id, nil, enabledResponse.Header.Get("ETag"), true)
	if deleted.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(deleted.Body)
		deleted.Body.Close()
		t.Fatalf("delete Service status/body = %d/%s", deleted.StatusCode, body)
	}
	deleted.Body.Close()
}

func TestServiceOpaquePaginationBindsTunnelAndFilters(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service pagination")
	createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "page-a", "page-a.example.test")
	createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "page-b", "page-b.example.test")

	path := "/api/v1/services?tunnel_id=" + tunnel.Tunnel.Id + "&page_size=1"
	response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, path, nil, "", false)
	var first ServiceList
	decodeSuccess(t, response, &first)
	if len(first.Items) != 1 || first.NextPageToken == nil {
		t.Fatalf("first Service page = %#v", first)
	}
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, path+"&page_token="+*first.NextPageToken, nil, "", false)
	terminalBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read terminal Service page: %v", err)
	}
	var second ServiceList
	if err := json.Unmarshal(terminalBody, &second); err != nil {
		t.Fatalf("decode terminal Service page: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].Id <= first.Items[0].Id || second.NextPageToken != nil ||
		bytes.Contains(terminalBody, []byte(`"next_page_token"`)) {
		t.Fatalf("second Service page = %#v after %#v", second, first)
	}

	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, path+"&enabled=false&page_token="+*first.NextPageToken, nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
	otherTunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "other pagination tunnel")
	response = doTunnelRequest(
		t, harness.tunnelAPIHarness, http.MethodGet,
		"/api/v1/services?tunnel_id="+otherTunnel.Tunnel.Id+"&page_size=1&page_token="+*first.NextPageToken,
		nil, "", false,
	)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, "/api/v1/tunnels?page_size=1&page_token="+*first.NextPageToken, nil, "", false)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDPAGETOKEN)
}

func TestServicePatchPreconditionsAndMergePatchMatrix(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service patch matrix")
	service, etag := createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "patch-service", "patch.example.test")
	_, otherETag := createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "other-service", "other.example.test")
	path := "/api/v1/services/" + service.Id

	response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "missing"}, "", true)
	assertAPIError(t, response, http.StatusPreconditionRequired, APIErrorCodePRECONDITIONREQUIRED)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "wrong"}, otherETag, true)
	assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "stale opaque"}, `"7"`, true)
	assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "malformed"}, `"bad tag"`, true)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDIFMATCH)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodDelete, path, nil, `"bad tag"`, true)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDIFMATCH)
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "unknown", "unknown": true}, etag, true)
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)

	for _, test := range []struct {
		name string
		body map[string]any
	}{
		{name: "name null", body: map[string]any{"name": nil}},
		{name: "origin null", body: map[string]any{"origin": nil}},
		{name: "proxy options null", body: map[string]any{"proxy_options": nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, test.body, etag, true)
			assertAPIError(t, response, http.StatusUnprocessableEntity, APIErrorCodeVALIDATIONFAILED)
		})
	}
	for _, group := range []struct {
		name   string
		fields []string
	}{
		{name: "origin", fields: []string{"scheme", "host", "port", "connect_timeout_ms", "tls_verify", "tls_server_name", "http_host_header"}},
		{name: "proxy_options", fields: []string{"disable_chunked_encoding", "disable_happy_eyeballs", "http_idle_connection_timeout_ms", "http_max_idle_connections", "tcp_keepalive_interval_ms"}},
		{name: "health", fields: []string{"type", "path", "interval_ms", "timeout_ms", "expected_status_min", "expected_status_max", "failure_threshold", "success_threshold"}},
		{name: "exposure", fields: []string{"type", "hostname", "path_prefix", "preserve_host", "public_port"}},
	} {
		for _, field := range group.fields {
			t.Run(group.name+"."+field+" null", func(t *testing.T) {
				body := map[string]any{group.name: map[string]any{field: nil}}
				response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, body, etag, true)
				assertAPIError(t, response, http.StatusUnprocessableEntity, APIErrorCodeVALIDATIONFAILED)
			})
		}
	}

	response = doTunnelRequest(
		t, harness.tunnelAPIHarness, http.MethodPatch, path,
		map[string]any{"origin": map[string]any{"host": "changed.example.test"}}, etag, true,
	)
	var originPatched Service
	decodeSuccess(t, response, &originPatched)
	oldETag := etag
	etag = response.Header.Get("ETag")
	httpOrigin, err := originPatched.Origin.AsHTTPOrigin()
	if err != nil || httpOrigin.Host != "changed.example.test" || httpOrigin.Port != 8080 || originPatched.Name != service.Name || originPatched.Exposure.IsNull() {
		t.Fatalf("origin-only PATCH changed omitted fields: origin=%#v service=%#v error=%v", httpOrigin, originPatched, err)
	}
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"name": "stale"}, oldETag, true)
	assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)

	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{
		"health": map[string]any{
			"type": "TCP", "interval_ms": 10000, "timeout_ms": 2000,
			"failure_threshold": 3, "success_threshold": 2,
		},
	}, etag, true)
	var healthEnabled Service
	decodeSuccess(t, response, &healthEnabled)
	etag = response.Header.Get("ETag")
	if healthEnabled.Health.IsNull() {
		t.Fatal("health value PATCH did not enable Health")
	}

	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{
		"exposure": map[string]any{"path_prefix": "/v2"},
	}, etag, true)
	var exposurePatched Service
	decodeSuccess(t, response, &exposurePatched)
	etag = response.Header.Get("ETag")
	exposure, err := exposurePatched.Exposure.Get()
	if err != nil {
		t.Fatalf("Exposure.Get() error = %v", err)
	}
	httpExposure, err := exposure.AsHTTPExposure()
	if err != nil || httpExposure.PathPrefix != "/v2" {
		t.Fatalf("exposure value PATCH = %#v, error = %v", httpExposure, err)
	}

	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"health": nil}, etag, true)
	var healthRemoved Service
	decodeSuccess(t, response, &healthRemoved)
	etag = response.Header.Get("ETag")
	if !healthRemoved.Health.IsNull() {
		t.Fatal("health null PATCH did not remove Health")
	}
	response = doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPatch, path, map[string]any{"exposure": nil}, etag, true)
	var exposureRemoved Service
	decodeSuccess(t, response, &exposureRemoved)
	if !exposureRemoved.Exposure.IsNull() {
		t.Fatal("exposure null PATCH did not remove Exposure")
	}
}

func TestUpdateServiceInputMapsEveryNestedValueAndOmitsSiblings(t *testing.T) {
	tests := []struct {
		group    string
		wire     string
		appField string
		value    any
	}{
		{group: "origin", wire: "scheme", appField: "Scheme", value: "https"},
		{group: "origin", wire: "host", appField: "Host", value: "origin.example.test"},
		{group: "origin", wire: "port", appField: "Port", value: 8443},
		{group: "origin", wire: "connect_timeout_ms", appField: "ConnectTimeoutMS", value: 5000},
		{group: "origin", wire: "tls_verify", appField: "TLSVerify", value: true},
		{group: "origin", wire: "tls_server_name", appField: "TLSServerName", value: "tls.example.test"},
		{group: "origin", wire: "http_host_header", appField: "HTTPHost", value: "host.example.test"},
		{group: "proxy_options", wire: "disable_chunked_encoding", appField: "DisableChunkedEncoding", value: true},
		{group: "proxy_options", wire: "disable_happy_eyeballs", appField: "DisableHappyEyeballs", value: true},
		{group: "proxy_options", wire: "http_idle_connection_timeout_ms", appField: "IdleConnectionTimeoutMS", value: 60000},
		{group: "proxy_options", wire: "http_max_idle_connections", appField: "MaxIdleConnections", value: 50},
		{group: "proxy_options", wire: "tcp_keepalive_interval_ms", appField: "TCPKeepAliveIntervalMS", value: 15000},
		{group: "health", wire: "type", appField: "Type", value: "HTTP"},
		{group: "health", wire: "path", appField: "Path", value: "/ready"},
		{group: "health", wire: "interval_ms", appField: "IntervalMS", value: 10000},
		{group: "health", wire: "timeout_ms", appField: "TimeoutMS", value: 2000},
		{group: "health", wire: "expected_status_min", appField: "ExpectedStatusMin", value: 200},
		{group: "health", wire: "expected_status_max", appField: "ExpectedStatusMax", value: 399},
		{group: "health", wire: "failure_threshold", appField: "FailureThreshold", value: 3},
		{group: "health", wire: "success_threshold", appField: "SuccessThreshold", value: 2},
		{group: "exposure", wire: "type", appField: "Type", value: "http"},
		{group: "exposure", wire: "hostname", appField: "Hostname", value: "public.example.test"},
		{group: "exposure", wire: "path_prefix", appField: "PathPrefix", value: "/v2"},
		{group: "exposure", wire: "preserve_host", appField: "PreserveHost", value: false},
		{group: "exposure", wire: "public_port", appField: "PublicPort", value: 20000},
	}
	identity := serviceMutationIdentity{tunnelID: "tun_01J00000000000000000000000", serviceVersion: 7, tunnelVersion: 5}
	for _, test := range tests {
		t.Run(test.group+"."+test.wire, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{test.group: map[string]any{test.wire: test.value}})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var body UpdateServiceRequest
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			input, err := updateServiceInput("svc_01J00000000000000000000000", identity, body)
			if err != nil {
				t.Fatalf("updateServiceInput() error = %v", err)
			}
			var patch any
			switch test.group {
			case "origin":
				patch = input.Origin
			case "proxy_options":
				patch = input.ProxyOptions
			case "health":
				patch = input.Health
			case "exposure":
				patch = input.Exposure
			default:
				t.Fatalf("unknown group %q", test.group)
			}
			value := reflect.ValueOf(patch)
			if value.Kind() != reflect.Pointer || value.IsNil() {
				t.Fatalf("%s patch = %#v", test.group, patch)
			}
			value = value.Elem()
			for index := 0; index < value.NumField(); index++ {
				field := value.Type().Field(index).Name
				isSet := !value.Field(index).IsNil()
				if isSet != (field == test.appField) {
					t.Fatalf("%s.%s set = %t, want only %s set", test.group, field, isSet, test.appField)
				}
			}
		})
	}
}

func TestServiceMutationPreconditionMatrixOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service precondition matrix")
	service, etag := createHTTPServiceForTest(
		t, harness, tunnel.Tunnel.Id, "precondition-service", "precondition.example.test",
	)
	createBody := map[string]any{
		"tunnel_id": tunnel.Tunnel.Id,
		"name":      "not-created",
		"origin": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 8081,
		},
		"exposure": map[string]any{"type": "http", "hostname": "not-created.example.test"},
	}
	basePath := "/api/v1/services/" + service.Id
	tests := []struct {
		name      string
		method    string
		path      string
		body      any
		staleETag string
	}{
		{name: "create", method: http.MethodPost, path: "/api/v1/services", body: createBody, staleETag: `"9"`},
		{name: "delete", method: http.MethodDelete, path: basePath, staleETag: `"7"`},
		{name: "enable", method: http.MethodPost, path: basePath + "/enable", staleETag: `"7"`},
		{name: "disable", method: http.MethodPost, path: basePath + "/disable", staleETag: `"7"`},
	}
	for _, test := range tests {
		t.Run(test.name+" missing", func(t *testing.T) {
			response := doTunnelRequest(t, harness.tunnelAPIHarness, test.method, test.path, test.body, "", true)
			assertAPIError(t, response, http.StatusPreconditionRequired, APIErrorCodePRECONDITIONREQUIRED)
		})
		t.Run(test.name+" stale", func(t *testing.T) {
			response := doTunnelRequest(t, harness.tunnelAPIHarness, test.method, test.path, test.body, test.staleETag, true)
			assertAPIError(t, response, http.StatusPreconditionFailed, APIErrorCodeRESOURCEVERSIONCONFLICT)
		})
	}

	response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, basePath, nil, "", false)
	if response.Header.Get("ETag") != etag {
		response.Body.Close()
		t.Fatalf("failed precondition matrix changed Service ETag = %q, want %q", response.Header.Get("ETag"), etag)
	}
	response.Body.Close()
}

func TestServicePatchRejectsJSONMediaTypeOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service media type")
	service, etag := createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "media-service", "media.example.test")
	request, err := http.NewRequest(
		http.MethodPatch,
		harness.server.URL+"/api/v1/services/"+service.Id,
		strings.NewReader(`{"name":"wrong media type"}`),
	)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", harness.publicURL)
	request.Header.Set("X-XTunnel-CSRF", harness.csrf)
	request.Header.Set("If-Match", etag)
	response, err := harness.client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
}

func TestServiceConcurrentPatchWithSameETagCommitsOnce(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service concurrent patch")
	service, etag := createHTTPServiceForTest(t, harness, tunnel.Tunnel.Id, "concurrent-service", "concurrent.example.test")
	path := "/api/v1/services/" + service.Id

	type patchResult struct {
		status int
		etag   string
		body   []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan patchResult, 2)
	var wait sync.WaitGroup
	for _, host := range []string{"winner-a.example.test", "winner-b.example.test"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			body, err := json.Marshal(map[string]any{"origin": map[string]string{"host": host}})
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
			request.Header.Set("Origin", harness.tunnelAPIHarness.publicURL)
			request.Header.Set("X-XTunnel-CSRF", harness.tunnelAPIHarness.csrf)
			request.Header.Set("If-Match", etag)
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
	var committedETag string
	var committed Service
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Service PATCH error = %v", result.err)
		}
		switch result.status {
		case http.StatusOK:
			succeeded++
			committedETag = result.etag
			if err := json.Unmarshal(result.body, &committed); err != nil {
				t.Fatalf("decode committed Service response: %v", err)
			}
		case http.StatusPreconditionFailed:
			conflicted++
		default:
			t.Fatalf("concurrent Service PATCH status = %d", result.status)
		}
	}
	if succeeded != 1 || conflicted != 1 || !validServiceIfMatch(committedETag) {
		t.Fatalf("concurrent PATCH results = succeeded:%d conflicted:%d ETag:%q", succeeded, conflicted, committedETag)
	}

	response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodGet, path, nil, "", false)
	if response.Header.Get("ETag") != committedETag {
		response.Body.Close()
		t.Fatalf("final Service ETag = %q, committed response ETag = %q", response.Header.Get("ETag"), committedETag)
	}
	var final Service
	decodeSuccess(t, response, &final)
	finalOrigin, err := final.Origin.AsHTTPOrigin()
	committedOrigin, committedErr := committed.Origin.AsHTTPOrigin()
	if err != nil || committedErr != nil || committed.Version != 2 || final.Version != committed.Version ||
		finalOrigin.Host != committedOrigin.Host ||
		finalOrigin.Host != "winner-a.example.test" && finalOrigin.Host != "winner-b.example.test" {
		t.Fatalf("final Service = version:%d origin:%#v error:%v", final.Version, finalOrigin, err)
	}

	var storedService repository.Service
	var storedTunnel repository.Tunnel
	var desiredState repository.RouteDesiredState
	if err := harness.tunnelAPIHarness.store.Read(context.Background(), func(view repository.RepositoryView) error {
		var err error
		storedService, err = view.Services().Get(context.Background(), tunnel.Tunnel.Id, service.Id)
		if err != nil {
			return err
		}
		storedTunnel, err = view.Tunnels().Get(context.Background(), tunnel.Tunnel.Id)
		if err != nil {
			return err
		}
		desiredState, err = view.Routes().LoadDesiredState(context.Background())
		return err
	}); err != nil {
		t.Fatalf("read concurrent Service result error = %v", err)
	}
	if storedService.Version != 2 || storedTunnel.DesiredRevision != 2 || desiredState.Generation != 2 {
		t.Fatalf(
			"concurrent Service versions = service:%d desired_revision:%d generation:%d, want 2/2/2",
			storedService.Version, storedTunnel.DesiredRevision, desiredState.Generation,
		)
	}
}

func TestServiceAPIRejectsNestedUnknownFieldsOverTLS(t *testing.T) {
	harness := newServiceAPIHarness(t)
	tunnel := createTunnelForTest(t, harness.tunnelAPIHarness, "service unknown fields")
	validCreateBody := func() map[string]any {
		return map[string]any{
			"tunnel_id": tunnel.Tunnel.Id,
			"name":      "strict service",
			"origin": map[string]any{
				"scheme": "http", "host": "origin.example.test", "port": 8080,
			},
			"exposure": map[string]any{"type": "http", "hostname": "strict.example.test"},
		}
	}
	createTests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "origin", mutate: func(body map[string]any) { body["origin"].(map[string]any)["unknown"] = true }},
		{name: "proxy options", mutate: func(body map[string]any) {
			body["proxy_options"] = map[string]any{"unknown": true}
		}},
		{name: "health", mutate: func(body map[string]any) {
			body["health"] = map[string]any{"type": "TCP", "unknown": true}
		}},
		{name: "exposure", mutate: func(body map[string]any) { body["exposure"].(map[string]any)["unknown"] = true }},
	}
	for _, test := range createTests {
		t.Run("create "+test.name, func(t *testing.T) {
			body := validCreateBody()
			test.mutate(body)
			response := doTunnelRequest(
				t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", body, `"1"`, true,
			)
			assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
		})
	}

	createdResponse := doTunnelRequest(
		t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", validCreateBody(), `"1"`, true,
	)
	if createdResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(createdResponse.Body)
		createdResponse.Body.Close()
		t.Fatalf("create valid Service status/body = %d/%s", createdResponse.StatusCode, body)
	}
	etag := createdResponse.Header.Get("ETag")
	var created Service
	decodeSuccess(t, createdResponse, &created)
	patches := []struct {
		name string
		body map[string]any
	}{
		{name: "origin", body: map[string]any{"origin": map[string]any{"unknown": true}}},
		{name: "proxy options", body: map[string]any{"proxy_options": map[string]any{"unknown": true}}},
		{name: "health", body: map[string]any{"health": map[string]any{"unknown": true}}},
		{name: "exposure", body: map[string]any{"exposure": map[string]any{"unknown": true}}},
	}
	for _, test := range patches {
		t.Run("patch "+test.name, func(t *testing.T) {
			response := doTunnelRequest(
				t, harness.tunnelAPIHarness, http.MethodPatch, "/api/v1/services/"+created.Id, test.body, etag, true,
			)
			assertAPIError(t, response, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST)
		})
	}
}

func TestServiceETagIsOpaqueAndBindsAggregateVersions(t *testing.T) {
	view := application.ServiceView{Service: repository.Service{ID: "svc_01ARZ3NDEKTSV4RRFFQ69G5FAV", TunnelID: "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV", Version: 7}, TunnelVersion: 11}
	etag := serviceETag(view)
	if !validServiceIfMatch(etag) || strings.Contains(etag, view.Service.ID) || strings.Contains(etag, view.Service.TunnelID) {
		t.Fatalf("serviceETag() = %q, want canonical opaque digest", etag)
	}
	changed := view
	changed.Service.Version++
	if serviceETag(changed) == etag {
		t.Fatal("serviceETag() did not bind Service version")
	}
	changed = view
	changed.TunnelVersion++
	if serviceETag(changed) == etag {
		t.Fatal("serviceETag() did not bind Tunnel version")
	}
	if !validServiceIfMatch(`"7"`) {
		t.Fatal("validServiceIfMatch() rejected a syntactically valid but stale opaque tag")
	}
	for _, value := range []string{"W/" + etag, `""`, `"bad tag"`, `*`, etag + "," + etag} {
		if validServiceIfMatch(value) {
			t.Fatalf("validServiceIfMatch(%q) = true", value)
		}
	}
}

func TestServiceUnionRejectsUnknownAndOverflowFields(t *testing.T) {
	var origin OriginInput
	if err := json.Unmarshal([]byte(`{"scheme":"http","host":"origin.example","port":80,"unknown":true}`), &origin); err != nil {
		t.Fatalf("json.Unmarshal(origin) error = %v", err)
	}
	if _, err := serviceOriginInput(origin, nil); err == nil {
		t.Fatal("serviceOriginInput() accepted unknown union field")
	}

	var exposure ExposureInput
	if err := json.Unmarshal([]byte(`{"type":"tcp","public_port":65536}`), &exposure); err != nil {
		t.Fatalf("json.Unmarshal(exposure) error = %v", err)
	}
	if _, err := createServiceExposure(exposure); err == nil {
		t.Fatal("createServiceExposure() accepted overflowing public_port")
	}
}

func TestMapServiceErrorContract(t *testing.T) {
	handler := &ManagementHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	tests := []struct {
		name   string
		err    error
		status int
		code   APIErrorCode
	}{
		{name: "TCP pool exhausted", err: tcpport.ErrPoolExhausted, status: 409, code: APIErrorCodeTCPPORTUNAVAILABLE},
		{name: "Exposure conflict", err: application.ErrServiceExposureConflict, status: 409, code: APIErrorCodeROUTECONFLICT},
		{name: "Service limit", err: serversnapshot.ErrServiceLimit, status: 422, code: APIErrorCodeTUNNELSERVICELIMIT},
		{name: "Health target budget", err: healthbudget.ErrTargetCapacity, status: 422, code: APIErrorCodeTUNNELSERVICELIMIT},
		{name: "Snapshot too large", err: serversnapshot.ErrSnapshotTooLarge, status: 422, code: APIErrorCodeSNAPSHOTTOOLARGE},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			failure := handler.mapServiceError(context.Background(), nil, test.err)
			if failure.status != test.status || failure.code != test.code {
				t.Fatalf("mapServiceError() = %d/%s, want %d/%s", failure.status, failure.code, test.status, test.code)
			}
		})
	}
}

type serviceAPIHarness struct {
	*tunnelAPIHarness
	budget *healthbudget.Manager
}

type serviceSnapshotGateFake struct{}

func (serviceSnapshotGateFake) Validate(string, int64, []repository.Service) error { return nil }

type serviceSnapshotNotifierFake struct{}

func (serviceSnapshotNotifierFake) MarkDirty(string) error { return nil }

type serviceRouteNotifierFake struct{}

func (serviceRouteNotifierFake) MarkDirty(uint64) {}

type serviceRuntimeFake struct{}

func (serviceRuntimeFake) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot { return nil }

type serviceLimitsFake struct{}

func (serviceLimitsFake) Snapshot() serverlimits.Snapshot {
	return serverlimits.Snapshot{ActiveByService: make(map[serverlimits.ConnectionService]uint64)}
}

type serviceApplyFailuresFake struct{}

func (serviceApplyFailuresFake) ServiceApplyFailure(string, uint64) *serverstatus.ApplyFailure {
	return nil
}

func createHTTPServiceForTest(
	t *testing.T,
	harness *serviceAPIHarness,
	tunnelID, name, hostname string,
) (Service, string) {
	t.Helper()
	body := map[string]any{
		"tunnel_id": tunnelID,
		"name":      name,
		"origin": map[string]any{
			"scheme": "http", "host": "127.0.0.1", "port": 8080,
		},
		"exposure": map[string]any{"type": "http", "hostname": hostname},
	}
	response := doTunnelRequest(t, harness.tunnelAPIHarness, http.MethodPost, "/api/v1/services", body, `"1"`, true)
	if response.StatusCode != http.StatusCreated || !validServiceIfMatch(response.Header.Get("ETag")) {
		responseBody, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create Service status/ETag/body = %d/%q/%s", response.StatusCode, response.Header.Get("ETag"), responseBody)
	}
	etag := response.Header.Get("ETag")
	var service Service
	decodeSuccess(t, response, &service)
	return service, etag
}

func newServiceAPIHarness(t *testing.T) *serviceAPIHarness {
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
	budget, err := healthbudget.New(healthbudget.Options{MaxTargetsPerTunnel: 100, MaxTargetsGlobal: 1000})
	if err != nil {
		t.Fatalf("healthbudget.New() error = %v", err)
	}
	policy, err := tcpport.New(20000, 20100, nil)
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	serviceOwner := application.NewServiceManagementService(store, serviceSnapshotGateFake{}, serviceSnapshotNotifierFake{}, budget)
	services := application.NewServiceAPIService(
		serviceOwner, policy, serviceRouteNotifierFake{}, serviceRuntimeFake{}, serviceLimitsFake{}, serviceApplyFailuresFake{},
	)

	server := httptest.NewUnstartedServer(nil)
	publicURL := "https://" + server.Listener.Addr().String()
	handler, err := NewHandler(HandlerOptions{
		Management: serverconfig.Management{PublicURL: publicURL}, Store: store,
		Tunnels:         application.NewTunnelManagementService(store, tokens, runtime, budget, endpoint, trust, 1000),
		Credentials:     application.NewCredentialLifecycleService(tokens, audit),
		TunnelLifecycle: application.NewTunnelLifecycleService(store, audit, runtime),
		Services:        services,
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
	return &serviceAPIHarness{tunnelAPIHarness: &tunnelAPIHarness{
		server: server, client: client, publicURL: publicURL, csrf: session.CsrfToken,
		runtime: runtime, store: store,
	}, budget: budget}
}
