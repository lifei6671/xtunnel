package httpingress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServerRejectsOversizeHeadersBeforeHandler(t *testing.T) {
	called := make(chan struct{}, 1)
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called <- struct{}{}
		}),
		MaxHeaderBytes: 128,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	connection, err := net.DialTimeout("tcp", server.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial HTTP ingress: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set HTTP ingress deadline: %v", err)
	}
	request := "GET / HTTP/1.1\r\nHost: example.test\r\nX-Oversize: " + strings.Repeat("x", 8192) + "\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatalf("write oversize request headers: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read oversize header response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("response status = %d, want 431", response.StatusCode)
	}
	select {
	case <-called:
		t.Fatal("oversize request reached Handler")
	default:
	}
}

func TestServerStopAcceptingPreservesActiveRequestUntilShutdown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(entered)
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	runtimeErrors := make(chan error, 1)
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Handler: handler, MaxHeaderBytes: 4096,
		ReportRuntimeError: func(err error) { runtimeErrors <- err },
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	clientTransport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: clientTransport}
	t.Cleanup(clientTransport.CloseIdleConnections)

	clientResult := make(chan error, 1)
	go func() {
		response, requestErr := client.Get("http://" + server.Addr().String() + "/stream")
		if requestErr != nil {
			clientResult <- requestErr
			return
		}
		defer response.Body.Close()
		_, readErr := io.Copy(io.Discard, response.Body)
		if response.StatusCode != http.StatusNoContent {
			clientResult <- errors.New("unexpected response status")
			return
		}
		clientResult <- readErr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter Handler")
	}
	if err := server.StopAccepting(); err != nil {
		t.Fatalf("StopAccepting() error = %v", err)
	}
	connection, dialErr := net.DialTimeout("tcp", server.Addr().String(), 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		t.Fatal("new connection succeeded after StopAccepting")
	}

	shutdownResult := make(chan error, 1)
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	go func() { shutdownResult <- server.Shutdown(shutdownContext) }()
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown() returned before active request completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-clientResult; err != nil {
		t.Fatalf("active request error = %v", err)
	}
	if err := <-shutdownResult; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() after Shutdown error = %v", err)
	}
	select {
	case err := <-runtimeErrors:
		t.Fatalf("normal shutdown reported runtime error: %v", err)
	default:
	}
}

func TestServerShutdownDeadlineCancelsBlockedHandlerAndWaitsForServeOwner(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	allowReturn := make(chan struct{})
	exited := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(entered)
		<-request.Context().Done()
		close(canceled)
		<-allowReturn
		close(exited)
	})
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Handler: handler, MaxHeaderBytes: 4096,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	clientTransport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: clientTransport}
	t.Cleanup(clientTransport.CloseIdleConnections)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, requestErr := client.Get("http://" + server.Addr().String() + "/blocked")
		if requestErr == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not enter Handler")
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShutdown()
	started := time.Now()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- server.Shutdown(shutdownContext) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("blocked Handler did not observe forced connection close")
	}
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown() returned before admitted Handler exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowReturn)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("blocked Handler did not exit after release")
	}
	err = <-shutdownResult
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Shutdown() elapsed = %v, want bounded force close plus Handler wait", elapsed)
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP client did not exit after forced connection close")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() after deadline error = %v", err)
	}
}

func TestServerRequestBodyIdleTimeoutSlidesOnEachRead(t *testing.T) {
	type readResult struct {
		bytes int64
		err   error
	}
	result := make(chan readResult, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		count, err := io.Copy(io.Discard, request.Body)
		result <- readResult{bytes: count, err: err}
		writer.WriteHeader(http.StatusRequestTimeout)
	})
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Handler: handler, MaxHeaderBytes: 4096,
		bodyIdleTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	connection, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("dial HTTP ingress: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := io.WriteString(connection, "POST /slow HTTP/1.1\r\nHost: example.test\r\nContent-Length: 4\r\n\r\na"); err != nil {
		t.Fatalf("write request headers/body: %v", err)
	}
	// 每个 60ms 间隔都小于 100ms，但总时长超过首个 Deadline；最终收到 3 字节
	// 证明窗口随成功 Read 推进，随后缺失第 4 字节才触发 idle timeout。
	for _, chunk := range []string{"b", "c"} {
		time.Sleep(60 * time.Millisecond)
		if _, err := io.WriteString(connection, chunk); err != nil {
			t.Fatalf("write sliding body chunk: %v", err)
		}
	}
	select {
	case observed := <-result:
		var networkErr net.Error
		if observed.bytes != 3 || !errors.As(observed.err, &networkErr) || !networkErr.Timeout() {
			t.Fatalf("body read = (%d, %v), want 3 bytes and timeout", observed.bytes, observed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow request body did not time out")
	}
}
