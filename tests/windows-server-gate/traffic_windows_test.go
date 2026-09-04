//go:build windows

package windowsservergate

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type origins struct{ httpPort, tcpPort int }

// Origin 的 accept goroutine 和连接 goroutine 均由测试等待；关闭 Listener 后先
// 等 accept 停止，再关已接收连接，最后等待处理器，避免清理与 WaitGroup.Add 竞争。
func startOrigins(t *testing.T) *origins {
	var httpWorkers sync.WaitGroup
	httpOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpWorkers.Add(1)
		defer httpWorkers.Done()
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			serveWebSocket(t, w, r)
			return
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 1024))
		if e != nil {
			t.Error("read HTTP origin body", e)
			w.WriteHeader(500)
			return
		}
		if r.Method != "POST" || r.URL.RequestURI() != "/echo?gate=1" || string(body) != "http-gate-request" || r.Host != "public.gate.test" {
			t.Error("origin received incorrect HTTP request")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		if _, e := io.WriteString(w, "windows-origin-response"); e != nil {
			t.Error("write HTTP origin", e)
		}
	}))
	t.Cleanup(func() { httpOrigin.Close(); httpWorkers.Wait() })
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	must(t, e, "start TCP origin")
	done := make(chan struct{})
	var workers sync.WaitGroup
	var lock sync.Mutex
	connections := map[net.Conn]bool{}
	go func() {
		defer close(done)
		for {
			connection, e := listener.Accept()
			if e != nil {
				return
			}
			lock.Lock()
			connections[connection] = true
			lock.Unlock()
			workers.Add(1)
			go func(c net.Conn) {
				defer workers.Done()
				defer func() { c.Close(); lock.Lock(); delete(connections, c); lock.Unlock() }()
				if e := c.SetDeadline(time.Now().Add(2 * time.Minute)); e != nil {
					return
				}
				var total []byte
				buffer := make([]byte, 1024)
				for {
					n, e := c.Read(buffer)
					if n > 0 {
						if len(total)+n > 4096 {
							return
						}
						total = append(total, buffer[:n]...)
						if _, we := c.Write(buffer[:n]); we != nil {
							return
						}
					}
					if e != nil {
						if e == io.EOF && !bytes.Contains(total, []byte("active-stop-proof")) {
							_, _ = c.Write([]byte("|origin-eof|"))
						}
						return
					}
				}
			}(connection)
		}
	}()
	t.Cleanup(func() {
		if e := listener.Close(); e != nil {
			t.Error("close TCP origin listener", e)
		}
		<-done
		lock.Lock()
		for c := range connections {
			c.Close()
		}
		lock.Unlock()
		workers.Wait()
	})
	return &origins{httpOrigin.Listener.Addr().(*net.TCPAddr).Port, listener.Addr().(*net.TCPAddr).Port}
}
func checkTraffic(t *testing.T, httpPort, tcpPort int) {
	t.Helper()
	checkHTTP(t, httpPort)
	checkWebSocket(t, httpPort)
	checkTCP(t, tcpPort)
}
func checkHTTP(t *testing.T, port int) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	r, e := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/echo?gate=1", port), strings.NewReader("http-gate-request"))
	must(t, e, "HTTP request")
	r.Host = "public.gate.test"
	response, e := client.Do(r)
	must(t, e, "public HTTP")
	body, e := io.ReadAll(io.LimitReader(response.Body, 1024))
	must(t, e, "HTTP response read")
	must(t, response.Body.Close(), "HTTP response close")
	if response.StatusCode != 200 || string(body) != "windows-origin-response" {
		t.Fatalf("HTTP product response mismatch: status=%d bytes=%d", response.StatusCode, len(body))
	}
}
func dialTCP(t *testing.T, port int) *net.TCPConn {
	t.Helper()
	c, e := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	must(t, e, "public TCP dial")
	tcp := c.(*net.TCPConn)
	must(t, tcp.SetDeadline(time.Now().Add(5*time.Second)), "TCP deadline")
	return tcp
}
func writeRead(t *testing.T, c net.Conn, data []byte) {
	t.Helper()
	_, e := c.Write(data)
	must(t, e, "TCP write")
	got := make([]byte, len(data))
	_, e = io.ReadFull(c, got)
	must(t, e, "TCP echo")
	if !bytes.Equal(got, data) {
		t.Fatal("TCP payload differs")
	}
}
func checkTCP(t *testing.T, port int) {
	t.Helper()
	c := dialTCP(t, port)
	defer c.Close()
	writeRead(t, c, []byte("tcp-gate\x00\xff"))
	must(t, c.CloseWrite(), "public TCP half-close")
	got, e := io.ReadAll(io.LimitReader(c, 1024))
	must(t, e, "read response after TCP half-close")
	if string(got) != "|origin-eof|" {
		t.Fatal("TCP reverse direction did not survive half-close")
	}
}
func webSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}
func serveWebSocket(t *testing.T, w http.ResponseWriter, r *http.Request) {
	c, rw, e := w.(http.Hijacker).Hijack()
	if e != nil {
		t.Error("origin WebSocket hijack", e)
		return
	}
	defer c.Close()
	if e := c.SetDeadline(time.Now().Add(5 * time.Second)); e != nil {
		t.Error("origin WebSocket deadline", e)
		return
	}
	_, e = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", webSocketAccept(r.Header.Get("Sec-WebSocket-Key")))
	if e != nil {
		t.Error("origin WebSocket handshake write", e)
		return
	}
	if e = rw.Flush(); e != nil {
		t.Error("origin WebSocket handshake flush", e)
		return
	}
	var header [2]byte
	if _, e = io.ReadFull(rw, header[:]); e != nil {
		t.Error("origin WebSocket header", e)
		return
	}
	if header != [2]byte{0x81, 0x87} {
		t.Error("origin WebSocket mask/length/opcode mismatch")
		return
	}
	var mask [4]byte
	payload := make([]byte, 7)
	if _, e = io.ReadFull(rw, mask[:]); e != nil {
		t.Error("origin WebSocket mask", e)
		return
	}
	if _, e = io.ReadFull(rw, payload); e != nil {
		t.Error("origin WebSocket payload", e)
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	if string(payload) != "ws-gate" {
		t.Error("origin WebSocket payload mismatch")
		return
	}
	if _, e = c.Write(append([]byte{0x81, 7}, payload...)); e != nil {
		t.Error("origin WebSocket response", e)
	}
}
func checkWebSocket(t *testing.T, port int) {
	t.Helper()
	c := dialTCP(t, port)
	defer c.Close()
	key := base64.StdEncoding.EncodeToString([]byte("windows-gate-key" + "!"))
	_, e := fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: public.gate.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", key)
	must(t, e, "public WebSocket request")
	reader := bufio.NewReader(c)
	response, e := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	must(t, e, "public WebSocket handshake")
	if response.StatusCode != 101 || response.Header.Get("Sec-WebSocket-Accept") != webSocketAccept(key) {
		response.Body.Close()
		t.Fatal("public WebSocket upgrade mismatch")
	}
	defer response.Body.Close()
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	frame := append([]byte{0x81, 0x87}, mask...)
	for i, b := range []byte("ws-gate") {
		frame = append(frame, b^mask[i%4])
	}
	_, e = c.Write(frame)
	must(t, e, "public WebSocket frame write")
	reply := make([]byte, 9)
	_, e = io.ReadFull(reader, reply)
	must(t, e, "public WebSocket frame read")
	if !bytes.Equal(reply, append([]byte{0x81, 7}, []byte("ws-gate")...)) {
		t.Fatal("WebSocket frame round trip mismatch")
	}
}
