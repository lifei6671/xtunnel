package httpingress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/lifei6671/xtunnel/internal/tunnel"
)

const oneGiB int64 = 1 << 30

// TestHandlerTransfersOneGiBInEachDirection 是 M4-03 的容量验收，不在每轮快速
// 单测或 Race 中重复搬运 2GiB；正式验收显式设置环境变量并记录命令。Reader 与
// ResponseWriter 都是常量空间，测试本身不会用内存掩盖整包缓冲问题。
func TestHandlerTransfersOneGiBInEachDirection(t *testing.T) {
	if os.Getenv("XTUNNEL_RUN_LARGE_STREAM_TEST") != "1" {
		t.Skip("set XTUNNEL_RUN_LARGE_STREAM_TEST=1 for the M4-03 1GiB streaming acceptance")
	}
	manager, _ := startRouteManager(t, baseHTTPRouteState(1))
	originResult := make(chan error, 1)
	dialer := dialerFunc(func(
		ctx context.Context,
		request tunnel.DialRequest,
	) (net.Conn, error) {
		server, peer := net.Pipe()
		go func() {
			defer peer.Close()
			request, err := http.ReadRequest(bufio.NewReader(peer))
			if err != nil {
				originResult <- err
				return
			}
			uploaded, err := io.Copy(io.Discard, request.Body)
			if closeErr := request.Body.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				originResult <- err
				return
			}
			if uploaded != oneGiB {
				originResult <- &byteCountError{direction: "upload", got: uploaded, want: oneGiB}
				return
			}
			response := &http.Response{
				StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
				Header: make(http.Header), ContentLength: oneGiB,
				Body: io.NopCloser(io.LimitReader(zeroReader{}, oneGiB)),
			}
			originResult <- response.Write(peer)
		}()
		return server, nil
	})
	handler := newTestHandler(t, manager, dialer)
	request := httptest.NewRequest(
		http.MethodPost, "/large", io.NopCloser(io.LimitReader(zeroReader{}, oneGiB)),
	)
	request.Host = "public.example.com"
	request.RequestURI = "/large"
	request.ContentLength = oneGiB
	request.RemoteAddr = "192.0.2.10:12345"
	writer := newCountingResponseWriter()
	handler.ServeHTTP(writer, request)
	if writer.status != http.StatusOK || writer.bytes != oneGiB {
		t.Fatalf("response = status %d bytes %d, want 200 and %d", writer.status, writer.bytes, oneGiB)
	}
	if err := <-originResult; err != nil {
		t.Fatalf("origin transfer error = %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

type countingResponseWriter struct {
	header http.Header
	status int
	bytes  int64
	mu     sync.Mutex
}

func newCountingResponseWriter() *countingResponseWriter {
	return &countingResponseWriter{header: make(http.Header)}
}

func (writer *countingResponseWriter) Header() http.Header { return writer.header }

func (writer *countingResponseWriter) WriteHeader(status int) {
	if writer.status == 0 {
		writer.status = status
	}
}

func (writer *countingResponseWriter) Write(buffer []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	writer.bytes += int64(len(buffer))
	return len(buffer), nil
}

func (*countingResponseWriter) Flush() {}

type byteCountError struct {
	direction string
	got       int64
	want      int64
}

func (err *byteCountError) Error() string {
	return fmt.Sprintf("%s byte count = %d, want %d", err.direction, err.got, err.want)
}
