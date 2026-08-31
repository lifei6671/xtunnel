package proxy

import (
	"io"
	"net"
	"sync"
	"testing"
)

const (
	proxyBenchmarkTransferBytes = 8 << 20
	proxyBenchmarkWriteBytes    = 64 << 10
)

type proxyBenchmarkReaderOnly struct {
	io.Reader
}

type proxyBenchmarkWriterOnly struct {
	io.Writer
}

type proxyBenchmarkCopy func(destination io.Writer, source io.Reader, limit int64) (int64, error)

// BenchmarkProxyBuffer 把 TCP 的 WriterTo/ReaderFrom 快路径与用户态 Buffer 路径分开。
// Generic 组显式隐藏接口快路径，确保 16/32/64 KiB 子项实际使用对应 Buffer，
// 避免把内核复制优化误判成 Buffer 尺寸收益。
func BenchmarkProxyBuffer(b *testing.B) {
	b.Run("io_copy_tcp_fast_path", func(b *testing.B) {
		benchmarkProxyCopy(b, func(destination io.Writer, source io.Reader, limit int64) (int64, error) {
			return io.Copy(destination, &io.LimitedReader{R: source, N: limit})
		})
	})

	b.Run("generic_32k_baseline", func(b *testing.B) {
		benchmarkProxyCopy(b, func(destination io.Writer, source io.Reader, limit int64) (int64, error) {
			return io.Copy(
				proxyBenchmarkWriterOnly{Writer: destination},
				&io.LimitedReader{R: proxyBenchmarkReaderOnly{Reader: source}, N: limit},
			)
		})
	})

	for _, benchmark := range []struct {
		name string
		size int
	}{
		{name: "16k", size: 16 << 10},
		{name: "32k", size: 32 << 10},
		{name: "64k", size: 64 << 10},
	} {
		b.Run("pooled_"+benchmark.name, func(b *testing.B) {
			pool := &sync.Pool{New: func() any {
				buffer := make([]byte, benchmark.size)
				return &buffer
			}}
			benchmarkProxyCopy(b, func(destination io.Writer, source io.Reader, limit int64) (int64, error) {
				buffer := pool.Get().(*[]byte)
				defer pool.Put(buffer)
				return io.CopyBuffer(
					proxyBenchmarkWriterOnly{Writer: destination},
					&io.LimitedReader{R: proxyBenchmarkReaderOnly{Reader: source}, N: limit},
					*buffer,
				)
			})
		})
	}
}

func benchmarkProxyCopy(b *testing.B, copyStream proxyBenchmarkCopy) {
	b.Helper()
	source, sourcePeer := proxyBenchmarkTCPPair(b)
	destination, destinationPeer := proxyBenchmarkTCPPair(b)
	if _, ok := any(source).(io.WriterTo); !ok {
		b.Fatal("benchmark TCP source does not implement io.WriterTo")
	}
	if _, ok := any(destination).(io.ReaderFrom); !ok {
		b.Fatal("benchmark TCP destination does not implement io.ReaderFrom")
	}

	totalBytes := int64(b.N) * proxyBenchmarkTransferBytes
	payload := make([]byte, proxyBenchmarkWriteBytes)
	for index := range payload {
		payload[index] = byte(index)
	}
	start := make(chan struct{})
	writeDone := make(chan error, 1)
	readDone := make(chan proxyBenchmarkReadResult, 1)
	go func() {
		<-start
		writeDone <- proxyBenchmarkWriteRepeated(sourcePeer, totalBytes, payload)
	}()
	go func() {
		<-start
		read, err := io.CopyN(io.Discard, destinationPeer, totalBytes)
		readDone <- proxyBenchmarkReadResult{bytes: read, err: err}
	}()

	b.SetBytes(proxyBenchmarkTransferBytes)
	b.ReportAllocs()
	b.ResetTimer()
	close(start)
	for range b.N {
		copied, err := copyStream(destination, source, proxyBenchmarkTransferBytes)
		if err != nil {
			b.Fatalf("copy %d bytes: %v", proxyBenchmarkTransferBytes, err)
		}
		if copied != proxyBenchmarkTransferBytes {
			b.Fatalf("copied bytes = %d, want %d", copied, proxyBenchmarkTransferBytes)
		}
	}
	writeErr := <-writeDone
	readResult := <-readDone
	b.StopTimer()
	if writeErr != nil {
		b.Fatalf("write benchmark payload: %v", writeErr)
	}
	if readResult.err != nil {
		b.Fatalf("read benchmark payload: %v", readResult.err)
	}
	if readResult.bytes != totalBytes {
		b.Fatalf("read bytes = %d, want %d", readResult.bytes, totalBytes)
	}
}

type proxyBenchmarkReadResult struct {
	bytes int64
	err   error
}

func proxyBenchmarkWriteRepeated(writer io.Writer, total int64, payload []byte) error {
	for total > 0 {
		chunk := payload
		if int64(len(chunk)) > total {
			chunk = chunk[:total]
		}
		written, err := writer.Write(chunk)
		if written > 0 {
			total -= int64(written)
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func proxyBenchmarkTCPPair(b *testing.B) (*net.TCPConn, *net.TCPConn) {
	b.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		b.Fatalf("ListenTCP: %v", err)
	}
	type dialResult struct {
		connection *net.TCPConn
		err        error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		connection, dialErr := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
		dialed <- dialResult{connection: connection, err: dialErr}
	}()
	accepted, acceptErr := listener.AcceptTCP()
	closeErr := listener.Close()
	peer := <-dialed
	if acceptErr != nil {
		if peer.connection != nil {
			_ = peer.connection.Close()
		}
		b.Fatalf("AcceptTCP: %v", acceptErr)
	}
	if peer.err != nil {
		_ = accepted.Close()
		b.Fatalf("DialTCP: %v", peer.err)
	}
	if closeErr != nil {
		_ = accepted.Close()
		_ = peer.connection.Close()
		b.Fatalf("close benchmark listener: %v", closeErr)
	}
	b.Cleanup(func() {
		_ = accepted.Close()
		_ = peer.connection.Close()
	})
	return accepted, peer.connection
}
