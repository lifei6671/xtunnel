package httpingress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	frontProxyE2EEnvironment = "XTUNNEL_RUN_FRONT_PROXY_E2E"
	frontProxyPublicHost     = "public.example.com"
	frontProxyOriginHost     = "virtual.origin.internal:9443"
	frontProxyHTTPSOrigin    = "https://https-client.example.com:8443"
	frontProxyWSSOrigin      = "https://websocket-client.example.com:9443"

	caddyImage = "caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648"
	nginxImage = "nginx:1.30.4-alpine@sha256:97d490c12ba55b4946b01546d1c3ed324e8d41ab1c9fcb2a616aa470620e5b46"
)

type frontProxyRuntime struct {
	name                string
	image               string
	configuration       string
	containerConfigPath string
	renderTemplate      bool
	command             []string
}

func TestFrontProxyDeployConfigurationPolicy(t *testing.T) {
	reverseProxyDir := frontProxyDeployDirectory(t)
	assertDeployConfiguration := func(name string, required []string, forbidden []string) {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(reverseProxyDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		configuration := string(content)
		for _, value := range required {
			if !strings.Contains(configuration, value) {
				t.Errorf("%s does not contain required policy %q", name, value)
			}
		}
		for _, value := range forbidden {
			if strings.Contains(configuration, value) {
				t.Errorf("%s contains forbidden policy %q", name, value)
			}
		}
	}

	assertDeployConfiguration("Caddyfile", []string{
		"read_header 10s",
		"flush_interval 100ms",
	}, []string{
		"read_timeout ",
		"write_timeout ",
		"flush_interval -",
	})
	assertDeployConfiguration("nginx.conf.template", []string{
		"client_header_timeout 10s;",
		"large_client_header_buffers 4 1m;",
		"client_max_body_size 0;",
		"proxy_read_timeout 1y;",
		"proxy_send_timeout 1y;",
	}, []string{
		"client_max_body_size 2g;",
		"proxy_read_timeout 1h;",
		"proxy_send_timeout 1h;",
	})
}

// TestFrontProxyHTTPSAndWebSocketE2E 只在显式 Gate 中运行。它直接启动固定摘要的
// 官方 Caddy/Nginx 容器并挂载 deploy 示例，禁止测试内维护第二份代理配置。
// --pull=never 保证缺失镜像立即失败，CI 必须在运行前显式拉取并记录同一摘要。
func TestFrontProxyHTTPSAndWebSocketE2E(t *testing.T) {
	if os.Getenv(frontProxyE2EEnvironment) != "1" {
		t.Skip("set XTUNNEL_RUN_FRONT_PROXY_E2E=1 after pre-pulling the pinned Caddy and Nginx images")
	}
	docker := requireFrontProxyDocker(t)
	reverseProxyDir := frontProxyDeployDirectory(t)
	runtimes := []frontProxyRuntime{
		{
			name: "caddy", image: caddyImage,
			configuration:       filepath.Join(reverseProxyDir, "Caddyfile"),
			containerConfigPath: "/etc/caddy/Caddyfile",
			command:             []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
		},
		{
			name: "nginx", image: nginxImage,
			configuration:       filepath.Join(reverseProxyDir, "nginx.conf.template"),
			containerConfigPath: "/etc/nginx/nginx.conf",
			renderTemplate:      true,
			command:             []string{"nginx", "-g", "daemon off;"},
		},
	}
	for _, proxyRuntime := range runtimes {
		t.Run(proxyRuntime.name, func(t *testing.T) {
			runFrontProxyRuntimeE2E(t, docker, proxyRuntime)
		})
	}
}

func runFrontProxyRuntimeE2E(t *testing.T, docker string, proxyRuntime frontProxyRuntime) {
	t.Helper()
	if _, err := os.Stat(proxyRuntime.configuration); err != nil {
		t.Fatalf("stat %s deploy configuration: %v", proxyRuntime.name, err)
	}
	inspectContext, cancelInspect := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelInspect()
	if output, err := exec.CommandContext(inspectContext, docker, "image", "inspect", proxyRuntime.image).CombinedOutput(); err != nil {
		t.Fatalf("pinned %s image is unavailable; pull it explicitly before the Gate: %v\n%s",
			proxyRuntime.name, err, output)
	}

	server, tunnelDialer := startFrontProxyIngress(t)
	publicPort := reserveFrontProxyPort(t)
	certificateFile, keyFile, roots := writeFrontProxyCertificate(t, frontProxyPublicHost)
	container := startFrontProxyContainer(t, docker, proxyRuntime, frontProxyContainerOptions{
		publicPort:      publicPort,
		upstream:        server.Addr().String(),
		certificateFile: certificateFile,
		keyFile:         keyFile,
	})
	proxyAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort))
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: frontProxyPublicHost,
	}
	waitForFrontProxyTLS(t, container, proxyAddress, tlsConfig)

	authority := net.JoinHostPort(frontProxyPublicHost, strconv.Itoa(publicPort))
	assertFrontProxyHTTPS(t, proxyAddress, authority, tlsConfig, tunnelDialer)
	assertFrontProxyWebSocket(t, proxyAddress, authority, tlsConfig, tunnelDialer)
	assertFrontProxyClientDisconnectCancelsUpstream(t, proxyAddress, authority, tlsConfig, tunnelDialer)

	calls := tunnelDialer.callsSnapshot()
	if len(calls) != 3 {
		t.Fatalf("Tunnel Dial calls = %d, want HTTPS, fresh WSS and disconnect requests: %+v", len(calls), calls)
	}
	for index, call := range calls {
		if call.TunnelID != testTunnelID || call.ServiceID != testServiceID || call.RequiredRevision != 1 ||
			call.Client != "127.0.0.1" {
			t.Fatalf("Tunnel Dial call %d = %+v, want exact route revision and normalized loopback client", index, call)
		}
	}
}

func startFrontProxyIngress(t *testing.T) (*Server, *upgradeDialer) {
	t.Helper()
	state := baseHTTPRouteState(1)
	state.Services[0].OriginHTTPHost = frontProxyOriginHost
	manager, _ := startRouteManager(t, state)
	dialer := newUpgradeDialer(pipeConnectionPair)
	handler := newTestHandlerWithTrustedProxies(t, manager, dialer, []string{"127.0.0.0/8", "::1/128"})
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Handler: handler, MaxHeaderBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Server.Close() error = %v", err)
		}
	})
	return server, dialer
}

func assertFrontProxyHTTPS(
	t *testing.T,
	proxyAddress string,
	authority string,
	tlsConfig *tls.Config,
	tunnelDialer *upgradeDialer,
) {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", proxyAddress)
		},
		TLSClientConfig:   tlsConfig.Clone(),
		ForceAttemptHTTP2: false,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://"+authority+"/front/proxy?source=https", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	setMaliciousForwardedHeaders(request.Header)
	request.Header.Set("Origin", frontProxyHTTPSOrigin)
	request.Close = true

	type clientResult struct {
		status int
		body   string
		err    error
	}
	clientDone := make(chan clientResult, 1)
	go func() {
		response, doErr := client.Do(request)
		if doErr != nil {
			clientDone <- clientResult{err: doErr}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		clientDone <- clientResult{
			status: response.StatusCode, body: string(body), err: errors.Join(readErr, closeErr),
		}
	}()

	origin := receiveFrontProxyOrigin(t, tunnelDialer)
	defer origin.Close()
	if err := origin.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set HTTPS Origin deadline: %v", err)
	}
	originRequest, err := http.ReadRequest(bufio.NewReader(origin))
	if err != nil {
		t.Fatalf("Origin read HTTPS request: %v", err)
	}
	if err := originRequest.Body.Close(); err != nil {
		t.Fatalf("close HTTPS Origin request body: %v", err)
	}
	if originRequest.Host != frontProxyOriginHost || originRequest.URL.Path != "/front/proxy" ||
		originRequest.URL.RawQuery != "source=https" {
		t.Fatalf("Origin HTTPS request = host %q path %q query %q",
			originRequest.Host, originRequest.URL.Path, originRequest.URL.RawQuery)
	}
	assertForwardedHeaders(t, originRequest.Header, "127.0.0.1", "https", authority)
	if values := originRequest.Header.Values("Origin"); len(values) != 1 || values[0] != frontProxyHTTPSOrigin {
		t.Fatalf("Origin HTTPS header = %v, want [%s]", values, frontProxyHTTPSOrigin)
	}
	responseBody := "front-proxy-https-ok"
	response := &http.Response{
		StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
		Body:   io.NopCloser(strings.NewReader(responseBody)), ContentLength: int64(len(responseBody)), Close: true,
	}
	if err := response.Write(origin); err != nil {
		t.Fatalf("Origin write HTTPS response: %v", err)
	}

	select {
	case result := <-clientDone:
		if result.err != nil || result.status != http.StatusOK || result.body != responseBody {
			t.Fatalf("HTTPS response = status %d body %q error %v", result.status, result.body, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HTTPS request did not finish through front proxy")
	}
}

func assertFrontProxyWebSocket(
	t *testing.T,
	proxyAddress string,
	authority string,
	tlsConfig *tls.Config,
	tunnelDialer *upgradeDialer,
) {
	t.Helper()
	client, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second}, "tcp", proxyAddress, tlsConfig.Clone(),
	)
	if err != nil {
		t.Fatalf("dial WSS front proxy: %v", err)
	}
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set WSS Client deadline: %v", err)
	}
	requestText := "GET /socket?source=wss HTTP/1.1\r\n" +
		"Host: " + authority + "\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Origin: " + frontProxyWSSOrigin + "\r\n" +
		maliciousForwardedHeaderText() + "\r\n"
	if _, err := io.WriteString(client, requestText); err != nil {
		t.Fatalf("write WSS handshake: %v", err)
	}

	origin := receiveFrontProxyOrigin(t, tunnelDialer)
	defer origin.Close()
	if err := origin.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set WSS Origin deadline: %v", err)
	}
	originReader := bufio.NewReader(origin)
	originRequest, err := http.ReadRequest(originReader)
	if err != nil {
		t.Fatalf("Origin read WSS handshake: %v", err)
	}
	if err := originRequest.Body.Close(); err != nil {
		t.Fatalf("close WSS Origin handshake body: %v", err)
	}
	if originRequest.Host != frontProxyOriginHost || originRequest.URL.Path != "/socket" ||
		originRequest.URL.RawQuery != "source=wss" {
		t.Fatalf("Origin WSS request = host %q path %q query %q",
			originRequest.Host, originRequest.URL.Path, originRequest.URL.RawQuery)
	}
	assertForwardedHeaders(t, originRequest.Header, "127.0.0.1", "https", authority)
	if values := originRequest.Header.Values("Origin"); len(values) != 1 || values[0] != frontProxyWSSOrigin {
		t.Fatalf("Origin WSS header = %v, want [%s]", values, frontProxyWSSOrigin)
	}
	responseText := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Accept: " + testWebSocketAccept + "\r\n\r\n"
	if _, err := io.WriteString(origin, responseText); err != nil {
		t.Fatalf("Origin write WSS handshake: %v", err)
	}
	clientReader := bufio.NewReader(client)
	response, err := http.ReadResponse(clientReader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("Client read WSS handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!httpgutsHeaderHasToken(response.Header.Values("Connection"), "upgrade") ||
		!strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("WSS response = status %d connection %v upgrade %q",
			response.StatusCode, response.Header.Values("Connection"), response.Header.Get("Upgrade"))
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear WSS Client deadline: %v", err)
	}
	if err := origin.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("clear WSS Origin deadline: %v", err)
	}

	clientFrame := maskedWebSocketFrame(0x1, []byte("client-through-wss"))
	if _, err := client.Write(clientFrame); err != nil {
		t.Fatalf("write WSS Client frame: %v", err)
	}
	assertReadBytes(t, origin, originReader, clientFrame, "Origin WSS Client frame")
	originFrame := unmaskedWebSocketFrame(0x1, []byte("origin-through-wss"))
	if _, err := origin.Write(originFrame); err != nil {
		t.Fatalf("write WSS Origin frame: %v", err)
	}
	assertReadBytes(t, client, clientReader, originFrame, "Client WSS Origin frame")
}

func assertFrontProxyClientDisconnectCancelsUpstream(
	t *testing.T,
	proxyAddress string,
	authority string,
	tlsConfig *tls.Config,
	tunnelDialer *upgradeDialer,
) {
	t.Helper()
	client, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second}, "tcp", proxyAddress, tlsConfig.Clone(),
	)
	if err != nil {
		t.Fatalf("dial HTTPS front proxy for disconnect: %v", err)
	}
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set disconnect Client deadline: %v", err)
	}
	requestText := "GET /disconnect HTTP/1.1\r\n" +
		"Host: " + authority + "\r\n" +
		"Connection: keep-alive\r\n\r\n"
	if _, err := io.WriteString(client, requestText); err != nil {
		_ = client.Close()
		t.Fatalf("write disconnect request: %v", err)
	}

	origin := receiveFrontProxyOrigin(t, tunnelDialer)
	defer origin.Close()
	if err := origin.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		_ = client.Close()
		t.Fatalf("set disconnect Origin deadline: %v", err)
	}
	originReader := bufio.NewReader(origin)
	originRequest, err := http.ReadRequest(originReader)
	if err != nil {
		_ = client.Close()
		t.Fatalf("Origin read disconnect request: %v", err)
	}
	if err := originRequest.Body.Close(); err != nil {
		_ = client.Close()
		t.Fatalf("close disconnect Origin request body: %v", err)
	}
	if _, err := io.WriteString(origin,
		"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n"+
			"5\r\nfirst\r\n"); err != nil {
		_ = client.Close()
		t.Fatalf("Origin write partial disconnect response: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = client.Close()
		t.Fatalf("Client read partial disconnect response: %v", err)
	}
	firstChunk := make([]byte, len("first"))
	if _, err := io.ReadFull(response.Body, firstChunk); err != nil {
		_ = client.Close()
		t.Fatalf("Client read first disconnect chunk: %v", err)
	}
	if string(firstChunk) != "first" {
		_ = client.Close()
		t.Fatalf("Client first disconnect chunk = %q, want first", firstChunk)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close disconnect Client: %v", err)
	}
	// 本用例故意先关闭底层连接模拟公网客户端中断；Body.Close 只释放本地解析状态，
	// 此时的读错误不影响要验证的 upstream 取消结果。
	_ = response.Body.Close()

	buffer := make([]byte, 1)
	if _, err := originReader.Read(buffer); err == nil {
		t.Fatal("upstream connection remained open after Client disconnect")
	} else if netError, ok := err.(net.Error); ok && netError.Timeout() {
		t.Fatal("upstream cancellation timed out after Client disconnect")
	}
}

func setMaliciousForwardedHeaders(header http.Header) {
	header.Set("Forwarded", `for="203.0.113.10";proto=http`)
	header.Set("X-Real-IP", "203.0.113.11")
	header.Set("X-Forwarded-For", "203.0.113.12")
	header.Set("X-Forwarded-Proto", "http")
	header.Set("X-Forwarded-Host", "attacker.invalid")
	header.Set("X-Forwarded-Unknown", "must-not-pass")
}

func maliciousForwardedHeaderText() string {
	return "Forwarded: for=203.0.113.10;proto=http\r\n" +
		"X-Real-IP: 203.0.113.11\r\n" +
		"X-Forwarded-For: 203.0.113.12\r\n" +
		"X-Forwarded-Proto: http\r\n" +
		"X-Forwarded-Host: attacker.invalid\r\n" +
		"X-Forwarded-Unknown: must-not-pass\r\n"
}

func receiveFrontProxyOrigin(t *testing.T, dialer *upgradeDialer) net.Conn {
	t.Helper()
	select {
	case origin := <-dialer.origins:
		return origin
	case <-time.After(10 * time.Second):
		t.Fatal("Tunnel Dial did not expose Origin connection")
		return nil
	}
}

type frontProxyContainerOptions struct {
	publicPort      int
	upstream        string
	certificateFile string
	keyFile         string
}

type frontProxyContainer struct {
	docker string
	name   string
	cmd    *exec.Cmd
	output *lockedBuffer
	done   chan struct{}

	mu      sync.Mutex
	waitErr error
	stop    sync.Once
}

func startFrontProxyContainer(
	t *testing.T,
	docker string,
	proxyRuntime frontProxyRuntime,
	options frontProxyContainerOptions,
) *frontProxyContainer {
	t.Helper()
	name := fmt.Sprintf("xtunnel-frontproxy-%s-%d-%d", proxyRuntime.name, os.Getpid(), time.Now().UnixNano())
	commonEnvironment := []string{
		"XTUNNEL_PUBLIC_HOST=" + frontProxyPublicHost,
		"XTUNNEL_HTTPS_PORT=" + strconv.Itoa(options.publicPort),
		"XTUNNEL_HTTP_INGRESS=" + options.upstream,
		"XTUNNEL_TLS_CERT_FILE=/xtunnel-certs/tls.crt",
		"XTUNNEL_TLS_KEY_FILE=/xtunnel-certs/tls.key",
	}
	configuration := proxyRuntime.configuration
	if proxyRuntime.renderTemplate {
		configuration = renderFrontProxyNginxConfig(t, configuration, map[string]string{
			"XTUNNEL_PUBLIC_HOST":   frontProxyPublicHost,
			"XTUNNEL_HTTPS_PORT":    strconv.Itoa(options.publicPort),
			"XTUNNEL_HTTP_INGRESS":  options.upstream,
			"XTUNNEL_TLS_CERT_FILE": "/xtunnel-certs/tls.crt",
			"XTUNNEL_TLS_KEY_FILE":  "/xtunnel-certs/tls.key",
		})
	}
	arguments := []string{
		"run", "--rm", "--pull=never", "--network=host", "--name", name,
		"--mount", dockerReadOnlyMount(configuration, proxyRuntime.containerConfigPath),
		"--mount", dockerReadOnlyMount(options.certificateFile, "/xtunnel-certs/tls.crt"),
		"--mount", dockerReadOnlyMount(options.keyFile, "/xtunnel-certs/tls.key"),
	}
	for _, value := range commonEnvironment {
		arguments = append(arguments, "--env", value)
	}
	arguments = append(arguments, proxyRuntime.image)
	arguments = append(arguments, proxyRuntime.command...)
	output := &lockedBuffer{}
	cmd := exec.Command(docker, arguments...)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s container: %v", proxyRuntime.name, err)
	}
	container := &frontProxyContainer{
		docker: docker, name: name, cmd: cmd, output: output, done: make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		container.mu.Lock()
		container.waitErr = err
		container.mu.Unlock()
		close(container.done)
	}()
	t.Cleanup(func() { container.stopAndWait(t) })
	return container
}

func (container *frontProxyContainer) stopAndWait(t *testing.T) {
	t.Helper()
	container.stop.Do(func() {
		// Cleanup 不能继承已取消的测试 Context；每条 Docker 命令使用独立的
		// 有界窗口，并最终确认 --rm 确实删除了 host-network 容器。
		select {
		case <-container.done:
			container.verifyRemoved(t)
			return
		default:
		}

		stopContext, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		stopOutput, stopErr := exec.CommandContext(
			stopContext, container.docker, "stop", "--time", "1", container.name,
		).CombinedOutput()
		cancelStop()
		if stopErr != nil {
			select {
			case <-container.done:
			default:
				t.Errorf("stop front proxy container %s: %v\n%s", container.name, stopErr, stopOutput)
			}
		}

		select {
		case <-container.done:
		case <-time.After(5 * time.Second):
			removeContext, cancelRemove := context.WithTimeout(context.Background(), 5*time.Second)
			removeOutput, removeErr := exec.CommandContext(
				removeContext, container.docker, "rm", "--force", container.name,
			).CombinedOutput()
			cancelRemove()
			if removeErr != nil {
				t.Errorf("force-remove front proxy container %s: %v\n%s", container.name, removeErr, removeOutput)
			}
			if container.cmd.Process != nil {
				if err := container.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("kill docker run process for %s: %v", container.name, err)
				}
			}
			select {
			case <-container.done:
			case <-time.After(2 * time.Second):
				t.Errorf("docker run process for %s did not exit", container.name)
			}
		}
		container.verifyRemoved(t)
	})
}

func (container *frontProxyContainer) verifyRemoved(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		inspectContext, cancelInspect := context.WithTimeout(context.Background(), 2*time.Second)
		output, err := exec.CommandContext(
			inspectContext, container.docker, "container", "inspect", container.name,
		).CombinedOutput()
		cancelInspect()
		if err != nil {
			message := strings.ToLower(string(output))
			if strings.Contains(message, "no such object") || strings.Contains(message, "no such container") {
				return
			}
			t.Errorf("verify front proxy container %s removal: %v\n%s", container.name, err, output)
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("front proxy container %s still exists after cleanup", container.name)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (container *frontProxyContainer) result() error {
	container.mu.Lock()
	defer container.mu.Unlock()
	return container.waitErr
}

func waitForFrontProxyTLS(t *testing.T, container *frontProxyContainer, address string, config *tls.Config) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, config.Clone())
		if err == nil {
			_ = connection.Close()
			return
		}
		select {
		case <-container.done:
			t.Fatalf("front proxy container exited before TLS readiness: %v\n%s",
				container.result(), container.output.String())
		case <-deadline.C:
			t.Fatalf("front proxy TLS readiness timed out\n%s", container.output.String())
		case <-ticker.C:
		}
	}
}

func requireFrontProxyDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("%s=1 requires Docker: %v", frontProxyE2EEnvironment, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Fatalf("%s=1 requires a reachable Docker daemon: %v\n%s", frontProxyE2EEnvironment, err, output)
	}
	return docker
}

func frontProxyDeployDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve front proxy test working directory: %v", err)
	}
	for {
		candidate := filepath.Join(directory, "deploy", "reverse-proxy")
		if _, err := os.Stat(filepath.Join(candidate, "Caddyfile")); err == nil {
			if _, err := os.Stat(filepath.Join(candidate, "nginx.conf.template")); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("find deploy/reverse-proxy from working directory %s", directory)
		}
		directory = parent
	}
}

func reserveFrontProxyPort(t *testing.T) int {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve front proxy port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved front proxy port: %v", err)
	}
	return port
}

func writeFrontProxyCertificate(t *testing.T, dnsName string) (string, string, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate front proxy CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "XTunnel M4-08 Test CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create front proxy CA certificate: %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse front proxy CA certificate: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate front proxy leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCertificate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create front proxy leaf certificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal front proxy leaf key: %v", err)
	}
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "tls.crt")
	keyFile := filepath.Join(directory, "tls.key")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certificateFile, certificatePEM, 0o644); err != nil {
		t.Fatalf("write front proxy certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write front proxy key: %v", err)
	}
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("front proxy key mode = %v, %v, want 0600", infoMode(info), err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	return certificateFile, keyFile, roots
}

func dockerReadOnlyMount(source string, destination string) string {
	return "type=bind,src=" + source + ",dst=" + destination + ",readonly"
}

func renderFrontProxyNginxConfig(t *testing.T, templatePath string, values map[string]string) string {
	t.Helper()
	content, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read Nginx deploy template: %v", err)
	}
	rendered := string(content)
	for name, value := range values {
		placeholder := "${" + name + "}"
		if !strings.Contains(rendered, placeholder) {
			t.Fatalf("Nginx deploy template does not contain %s", placeholder)
		}
		rendered = strings.ReplaceAll(rendered, placeholder, value)
	}
	if strings.Contains(rendered, "${XTUNNEL_") {
		t.Fatalf("Nginx deploy template still contains an unresolved XTunnel placeholder")
	}
	path := filepath.Join(t.TempDir(), "nginx.conf")
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		t.Fatalf("write rendered Nginx configuration: %v", err)
	}
	return path
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
