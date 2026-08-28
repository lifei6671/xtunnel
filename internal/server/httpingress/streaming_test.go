package httpingress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

func TestHandlerStreamsRequestBodyBeforeReadingItAll(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	bodyRelease := make(chan struct{})
	body := newStagedBody([]byte("first-"), []byte("second"), bodyRelease)
	originReadFirst := make(chan struct{})
	originResult := make(chan string, 1)
	originErrors := make(chan error, 1)
	dialer := dialerFunc(func(ctx context.Context, _ string, _ string, _ uint64, ingress protocolv1.IngressType, _ string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if ingress != protocolv1.IngressType_INGRESS_TYPE_HTTP {
			return nil, errors.New("unexpected ingress type")
		}
		server, origin := net.Pipe()
		go func() {
			defer origin.Close()
			request, err := http.ReadRequest(bufio.NewReader(origin))
			if err != nil {
				originErrors <- err
				return
			}
			first := make([]byte, len("first-"))
			if _, err := io.ReadFull(request.Body, first); err != nil {
				originErrors <- err
				return
			}
			close(originReadFirst)
			rest, err := io.ReadAll(request.Body)
			if err != nil {
				originErrors <- err
				return
			}
			if err := request.Body.Close(); err != nil {
				originErrors <- err
				return
			}
			originResult <- string(first) + string(rest)
			responseBody := "uploaded"
			response := &http.Response{
				StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
				Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(responseBody)),
				ContentLength: int64(len(responseBody)),
			}
			if err := response.Write(origin); err != nil {
				originErrors <- err
			}
		}()
		return server, nil
	})
	handler := newTestHandler(t, manager, dialer)
	request := httptest.NewRequest(http.MethodPost, "/upload", body)
	request.Host = "public.example.com"
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	// 在释放第二段输入前，Origin 必须已经读到第一段；否则实现正在整包缓冲请求。
	select {
	case <-originReadFirst:
	case err := <-originErrors:
		t.Fatalf("origin failed before first streamed chunk: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("origin did not receive first request chunk before the remaining body was available")
	}
	close(bodyRelease)
	select {
	case got := <-originResult:
		if got != "first-second" {
			t.Fatalf("origin body = %q, want first-second", got)
		}
	case err := <-originErrors:
		t.Fatalf("origin failed while reading streamed body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("origin did not finish streamed request")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not finish streamed request")
	}
	if response.Code != http.StatusOK || response.Body.String() != "uploaded" {
		t.Fatalf("response = (%d, %q), want (200, uploaded)", response.Code, response.Body.String())
	}
}

func TestHandlerStreamsResponseBodyBeforeOriginFinishes(t *testing.T) {
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	originWroteFirst := make(chan struct{})
	originRelease := make(chan struct{})
	originDone := make(chan error, 1)
	dialer := dialerFunc(func(ctx context.Context, _ string, _ string, _ uint64, _ protocolv1.IngressType, _ string) (net.Conn, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		server, origin := net.Pipe()
		go func() {
			defer origin.Close()
			request, err := http.ReadRequest(bufio.NewReader(origin))
			if err != nil {
				originDone <- err
				return
			}
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				originDone <- err
				return
			}
			if err := request.Body.Close(); err != nil {
				originDone <- err
				return
			}
			if _, err := io.WriteString(origin, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nfirst\r\n"); err != nil {
				originDone <- err
				return
			}
			close(originWroteFirst)
			<-originRelease
			_, err = io.WriteString(origin, "6\r\nsecond\r\n0\r\n\r\n")
			originDone <- err
		}()
		return server, nil
	})
	handler := newTestHandler(t, manager, dialer)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/stream", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	request.Host = "public.example.com"
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close streamed response: %v", err)
		}
	}()

	select {
	case <-originWroteFirst:
	case err := <-originDone:
		t.Fatalf("origin failed before first response chunk: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("origin did not write first response chunk")
	}
	firstResult := make(chan struct {
		body string
		err  error
	}, 1)
	go func() {
		first := make([]byte, len("first"))
		_, readErr := io.ReadFull(response.Body, first)
		firstResult <- struct {
			body string
			err  error
		}{body: string(first), err: readErr}
	}()
	select {
	case result := <-firstResult:
		if result.err != nil || result.body != "first" {
			t.Fatalf("first streamed response = (%q, %v), want (first, nil)", result.body, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive first response chunk before Origin completed")
	}
	close(originRelease)
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining streamed response: %v", err)
	}
	if string(rest) != "second" {
		t.Fatalf("remaining streamed response = %q, want second", rest)
	}
	select {
	case err := <-originDone:
		if err != nil {
			t.Fatalf("origin response writer error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin response writer did not finish")
	}
}

type stagedBody struct {
	first     *bytes.Reader
	second    *bytes.Reader
	release   <-chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	released  bool
}

func newStagedBody(first, second []byte, release <-chan struct{}) *stagedBody {
	return &stagedBody{
		first: bytes.NewReader(first), second: bytes.NewReader(second),
		release: release, closed: make(chan struct{}),
	}
}

func (body *stagedBody) Read(buffer []byte) (int, error) {
	if body.first.Len() > 0 {
		return body.first.Read(buffer)
	}
	if !body.released {
		select {
		case <-body.release:
			body.released = true
		case <-body.closed:
			return 0, io.ErrClosedPipe
		}
	}
	return body.second.Read(buffer)
}

func (body *stagedBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}
