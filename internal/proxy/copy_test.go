package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

func TestCopyProxyStreamPreservesFastPathPriorityWithoutUsingPool(t *testing.T) {
	pool := &trackingProxyBufferPool{buffer: new(proxyCopyBuffer)}

	t.Run("WriterTo takes priority over ReaderFrom", func(t *testing.T) {
		writeToErr := errors.New("writer-to result")
		source := &trackingWriterToReader{payload: []byte("writer-to"), err: writeToErr}
		destination := &trackingReaderFromWriter{}

		copied, err := copyProxyStream(destination, source, pool)
		if !errors.Is(err, writeToErr) {
			t.Fatalf("copyProxyStream() error = %v, want WriterTo result", err)
		}
		if copied != int64(len(source.payload)) {
			t.Fatalf("copyProxyStream() bytes = %d, want %d", copied, len(source.payload))
		}
		if source.writeToCalls != 1 || source.readCalls != 0 {
			t.Fatalf("source calls: WriteTo=%d Read=%d, want 1/0", source.writeToCalls, source.readCalls)
		}
		if destination.readFromCalls != 0 {
			t.Fatalf("destination ReadFrom calls = %d, want 0", destination.readFromCalls)
		}
		if !bytes.Equal(destination.bytes.Bytes(), source.payload) {
			t.Fatalf("destination bytes = %q, want %q", destination.bytes.Bytes(), source.payload)
		}
		if pool.getCalls != 0 || pool.putCalls != 0 {
			t.Fatalf("generic pool calls: Get=%d Put=%d, want 0/0", pool.getCalls, pool.putCalls)
		}
	})

	t.Run("ReaderFrom is used when WriterTo is absent", func(t *testing.T) {
		payload := []byte("reader-from")
		readFromErr := errors.New("reader-from result")
		source := proxyCopyReaderOnly{Reader: bytes.NewReader(payload)}
		destination := &trackingReaderFromWriter{err: readFromErr}

		copied, err := copyProxyStream(destination, source, pool)
		if !errors.Is(err, readFromErr) {
			t.Fatalf("copyProxyStream() error = %v, want ReaderFrom result", err)
		}
		if copied != int64(len(payload)) {
			t.Fatalf("copyProxyStream() bytes = %d, want %d", copied, len(payload))
		}
		if destination.readFromCalls != 1 || destination.writeCalls != 0 {
			t.Fatalf("destination calls: ReadFrom=%d Write=%d, want 1/0", destination.readFromCalls, destination.writeCalls)
		}
		if !bytes.Equal(destination.bytes.Bytes(), payload) {
			t.Fatalf("destination bytes = %q, want %q", destination.bytes.Bytes(), payload)
		}
		if pool.getCalls != 0 || pool.putCalls != 0 {
			t.Fatalf("generic pool calls: Get=%d Put=%d, want 0/0", pool.getCalls, pool.putCalls)
		}
	})
}

func TestCopyProxyStreamGenericFallbackUsesAndReturns32KiBBuffer(t *testing.T) {
	readErr := errors.New("injected read failure")
	writeErr := errors.New("injected write failure")
	payload := bytes.Repeat([]byte("generic-buffer-"), 4096)
	for _, test := range []struct {
		name       string
		writer     io.Writer
		readerErr  error
		wantErr    error
		wantCopied int64
	}{
		{name: "success", writer: &bytes.Buffer{}, wantCopied: int64(len(payload))},
		{name: "read error", writer: &bytes.Buffer{}, readerErr: readErr, wantErr: readErr, wantCopied: int64(len(payload))},
		{name: "write error", writer: errorWriter{err: writeErr}, wantErr: writeErr},
		{name: "short write", writer: shortWriter{}, wantErr: io.ErrShortWrite, wantCopied: proxyCopyBufferSize - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			buffer := new(proxyCopyBuffer)
			pool := &trackingProxyBufferPool{buffer: buffer}
			source := &recordingReader{payload: payload, terminalErr: test.readerErr}

			copied, err := copyProxyStream(
				proxyCopyWriterOnly{Writer: test.writer},
				proxyCopyReaderOnly{Reader: source},
				pool,
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("copyProxyStream() error = %v, want %v", err, test.wantErr)
			}
			if copied != test.wantCopied {
				t.Fatalf("copyProxyStream() bytes = %d, want %d", copied, test.wantCopied)
			}
			if source.firstBufferSize != proxyCopyBufferSize || source.maxBufferSize != proxyCopyBufferSize {
				t.Fatalf("generic read buffer sizes: first=%d max=%d, want %d", source.firstBufferSize, source.maxBufferSize, proxyCopyBufferSize)
			}
			if pool.getCalls != 1 || pool.putCalls != 1 || pool.returned != buffer {
				t.Fatalf("pool lifecycle: Get=%d Put=%d same=%t, want 1/1/true", pool.getCalls, pool.putCalls, pool.returned == buffer)
			}
		})
	}
}

func TestCopyProxyStreamGenericFallbackReturnsBufferOnPanic(t *testing.T) {
	buffer := new(proxyCopyBuffer)
	pool := &trackingProxyBufferPool{buffer: buffer}
	panicValue := errors.New("injected read panic")

	func() {
		defer func() {
			if recovered := recover(); recovered != panicValue {
				t.Fatalf("recovered panic = %v, want %v", recovered, panicValue)
			}
		}()
		_, _ = copyProxyStream(
			proxyCopyWriterOnly{Writer: io.Discard},
			proxyCopyReaderOnly{Reader: panicReader{value: panicValue}},
			pool,
		)
	}()

	if pool.getCalls != 1 || pool.putCalls != 1 || pool.returned != buffer {
		t.Fatalf("pool lifecycle after panic: Get=%d Put=%d same=%t, want 1/1/true", pool.getCalls, pool.putCalls, pool.returned == buffer)
	}
}

func TestCopyProxyStreamConcurrentGenericFallback(t *testing.T) {
	const (
		workers    = 32
		iterations = 20
	)
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := range workers {
		go func() {
			defer wait.Done()
			payload := bytes.Repeat([]byte{byte(worker)}, (64<<10)+17)
			for iteration := range iterations {
				var destination bytes.Buffer
				copied, err := copyProxyStream(
					proxyCopyWriterOnly{Writer: &destination},
					proxyCopyReaderOnly{Reader: bytes.NewReader(payload)},
					&proxyCopyBuffers,
				)
				if err != nil {
					errorsByWorker <- fmt.Errorf("worker %d iteration %d: generic copy: %w", worker, iteration, err)
					return
				}
				if copied != int64(len(payload)) || !bytes.Equal(destination.Bytes(), payload) {
					errorsByWorker <- fmt.Errorf("worker %d iteration %d: generic copy changed payload", worker, iteration)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}
}

type proxyCopyReaderOnly struct {
	io.Reader
}

type proxyCopyWriterOnly struct {
	io.Writer
}

type trackingWriterToReader struct {
	payload      []byte
	err          error
	readCalls    int
	writeToCalls int
}

func (reader *trackingWriterToReader) Read([]byte) (int, error) {
	reader.readCalls++
	return 0, errors.New("Read must not be called when WriterTo is available")
}

func (reader *trackingWriterToReader) WriteTo(writer io.Writer) (int64, error) {
	reader.writeToCalls++
	written, err := writer.Write(reader.payload)
	if err != nil {
		return int64(written), err
	}
	return int64(written), reader.err
}

type trackingReaderFromWriter struct {
	bytes         bytes.Buffer
	err           error
	readFromCalls int
	writeCalls    int
}

func (writer *trackingReaderFromWriter) Write(payload []byte) (int, error) {
	writer.writeCalls++
	return writer.bytes.Write(payload)
}

func (writer *trackingReaderFromWriter) ReadFrom(reader io.Reader) (int64, error) {
	writer.readFromCalls++
	payload, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}
	written, err := writer.bytes.Write(payload)
	if err != nil {
		return int64(written), err
	}
	return int64(written), writer.err
}

type recordingReader struct {
	payload         []byte
	offset          int
	terminalErr     error
	firstBufferSize int
	maxBufferSize   int
}

func (reader *recordingReader) Read(buffer []byte) (int, error) {
	if reader.firstBufferSize == 0 {
		reader.firstBufferSize = len(buffer)
	}
	if len(buffer) > reader.maxBufferSize {
		reader.maxBufferSize = len(buffer)
	}
	if reader.offset == len(reader.payload) {
		if reader.terminalErr != nil {
			return 0, reader.terminalErr
		}
		return 0, io.EOF
	}
	written := copy(buffer, reader.payload[reader.offset:])
	reader.offset += written
	return written, nil
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

type shortWriter struct{}

func (shortWriter) Write(payload []byte) (int, error) {
	return len(payload) - 1, nil
}

type panicReader struct {
	value any
}

func (reader panicReader) Read([]byte) (int, error) {
	panic(reader.value)
}

type trackingProxyBufferPool struct {
	buffer   *proxyCopyBuffer
	returned any
	getCalls int
	putCalls int
}

func (pool *trackingProxyBufferPool) Get() any {
	pool.getCalls++
	return pool.buffer
}

func (pool *trackingProxyBufferPool) Put(buffer any) {
	pool.putCalls++
	pool.returned = buffer
}
