package metrics

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerExposesOnlyConfiguredPathAndShutsDown(t *testing.T) {
	registry, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Path: "/internal/metrics", Registry: registry,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	})

	baseURL := "http://" + server.Addr().String()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(baseURL + "/internal/metrics")
	if err != nil {
		t.Fatalf("GET metrics error = %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close metrics response errors = %v / %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET metrics status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(string(body), "xtunnel_connectors_online") {
		t.Fatalf("GET metrics body does not contain frozen metric: %s", body)
	}

	for _, path := range []string{"/", "/metrics", "/internal/metrics/", "//internal/metrics", "/internal/../internal/metrics"} {
		otherResponse, requestErr := client.Get(baseURL + path)
		if requestErr != nil {
			t.Fatalf("GET %s error = %v", path, requestErr)
		}
		if closeBodyErr := otherResponse.Body.Close(); closeBodyErr != nil {
			t.Fatalf("close GET %s body: %v", path, closeBodyErr)
		}
		if otherResponse.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, otherResponse.StatusCode)
		}
	}

	if err := server.StopAccepting(); err != nil {
		t.Fatalf("StopAccepting() error = %v", err)
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	connection, err := net.DialTimeout("tcp", server.Addr().String(), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("metrics listener still accepts connections after Shutdown")
	}
}

func TestStopAcceptingRejectsRequestOnExistingConnection(t *testing.T) {
	registry, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	server, err := NewServer(ServerOptions{Listen: "127.0.0.1:0", Path: "/metrics", Registry: registry})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	connection, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	request := "GET /metrics HTTP/1.1\r\nHost: metrics.test\r\nConnection: keep-alive\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	first, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	if _, err := io.Copy(io.Discard, first.Body); err != nil {
		t.Fatalf("drain first response: %v", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first response status = %d, want 200", first.StatusCode)
	}

	if err := server.StopAccepting(); err != nil {
		t.Fatalf("StopAccepting() error = %v", err)
	}
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatalf("write second request on KeepAlive connection: %v", err)
	}
	second, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read second response: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second response status = %d, want 503", second.StatusCode)
	}
}

func TestShutdownWaitsForAdmittedScrape(t *testing.T) {
	source := newBlockingOwnerSource()
	t.Cleanup(source.unblock)
	registry, err := NewRegistry(source)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	server, err := NewServer(ServerOptions{Listen: "127.0.0.1:0", Path: "/metrics", Registry: registry})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + server.Addr().String() + "/metrics")
		if requestErr == nil {
			requestErr = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("metrics scrape did not enter OwnerSource")
	}

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownDone <- server.Shutdown(ctx)
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown() returned before admitted scrape completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	source.unblock()
	if err := <-requestDone; err != nil {
		t.Fatalf("metrics request error = %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestServerRejectsInvalidOptionsAndBindFailure(t *testing.T) {
	registry, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	for _, test := range []struct {
		name    string
		options ServerOptions
	}{
		{name: "missing listen", options: ServerOptions{Path: "/metrics", Registry: registry}},
		{name: "relative path", options: ServerOptions{Listen: "127.0.0.1:0", Path: "metrics", Registry: registry}},
		{name: "missing registry", options: ServerOptions{Listen: "127.0.0.1:0", Path: "/metrics"}},
		{name: "invalid mux pattern", options: ServerOptions{Listen: "127.0.0.1:0", Path: "/metrics/{", Registry: registry}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewServer(test.options); err == nil {
				t.Fatal("NewServer() error = nil, want failure")
			}
		})
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	defer listener.Close()
	server, err := NewServer(ServerOptions{Listen: listener.Addr().String(), Path: "/metrics", Registry: registry})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "listen for Prometheus metrics") {
		t.Fatalf("Start() error = %v, want bind failure", err)
	}
}

func TestCloseBeforeStartPreventsLaterStart(t *testing.T) {
	registry, err := NewRegistry(staticOwnerSource{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	server, err := NewServer(ServerOptions{Listen: "127.0.0.1:0", Path: "/metrics", Registry: registry})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil after Close")
	}
}

type blockingOwnerSource struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingOwnerSource() *blockingOwnerSource {
	return &blockingOwnerSource{entered: make(chan struct{}), release: make(chan struct{})}
}

func (source *blockingOwnerSource) MetricsOwnerSnapshot() OwnerSnapshot {
	source.enteredOnce.Do(func() { close(source.entered) })
	<-source.release
	return OwnerSnapshot{}
}

func (source *blockingOwnerSource) unblock() {
	source.releaseOnce.Do(func() { close(source.release) })
}

func TestNormalizeClosedError(t *testing.T) {
	if err := normalizeClosedError(net.ErrClosed); err != nil {
		t.Fatalf("normalizeClosedError(net.ErrClosed) = %v", err)
	}
	want := errors.New("boom")
	if got := normalizeClosedError(want); !errors.Is(got, want) {
		t.Fatalf("normalizeClosedError() = %v, want %v", got, want)
	}
}
