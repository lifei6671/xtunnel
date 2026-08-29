package managementapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServerLifecycle(t *testing.T) {
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
		MaxHeaderBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if server.server.ReadTimeout != managementReadTimeout || server.server.WriteTimeout != managementWriteTimeout || server.server.IdleTimeout != managementIdleTimeout {
		t.Fatalf("management timeouts = read %s, write %s, idle %s", server.server.ReadTimeout, server.server.WriteTimeout, server.server.IdleTimeout)
	}
	response, err := http.Get("http://" + server.Addr().String())
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() after Shutdown error = %v", err)
	}
}

func TestServerReadTimeoutStopsSlowBody(t *testing.T) {
	bodyResult := make(chan error, 1)
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, readErr := io.Copy(io.Discard, request.Body)
			bodyResult <- readErr
			writer.WriteHeader(http.StatusBadRequest)
		}),
		MaxHeaderBytes: 1 << 20,
		readTimeout:    50 * time.Millisecond,
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
		t.Fatalf("net.Dial() error = %v", err)
	}
	defer connection.Close()
	if _, err := fmt.Fprint(connection, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 10\r\n\r\nx"); err != nil {
		t.Fatalf("write partial request error = %v", err)
	}
	select {
	case readErr := <-bodyResult:
		if readErr == nil {
			t.Fatal("slow request body read error = nil")
		}
	case <-time.After(time.Second):
		t.Fatal("slow request body was not interrupted by ReadTimeout")
	}
}

func TestServerShutdownWaitsForActiveHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			writer.WriteHeader(http.StatusNoContent)
		}),
		MaxHeaderBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.Get("http://" + server.Addr().String())
		if response != nil {
			response.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not enter Handler")
	}
	shutdownDone := make(chan error, 1)
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelShutdown()
	go func() { shutdownDone <- server.Shutdown(shutdownContext) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before active Handler: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-shutdownDone:
		if err == nil {
			t.Fatal("Shutdown after deadline error = nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after Handler exited")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("client request did not finish")
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}
