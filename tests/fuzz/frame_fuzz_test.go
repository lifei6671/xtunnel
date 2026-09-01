package fuzz_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
)

const (
	fuzzUVarintPayloadLimit uint64 = 64
	fuzzFrameLimit          uint64 = 4 << 10
)

func FuzzUVarintDecoder(f *testing.F) {
	seeds := [][]byte{
		{}, {0x00}, {0x01}, {0x7f}, {0x80, 0x01}, {0xff, 0x7f},
		{0x80, 0x80, 0x01},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
		{0x80, 0x00}, {0x81, 0x00}, {0xff, 0x00}, {0x80},
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02},
		{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64 {
			return
		}
		reader := bytes.NewReader(data)
		payload, err := frame.ReadPayload(reader, fuzzUVarintPayloadLimit)
		length, prefixSize := binary.Uvarint(data)
		if prefixSize <= 0 {
			assertPayloadReadError(t, err)
			return
		}

		var canonicalPrefix [binary.MaxVarintLen64]byte
		canonicalSize := binary.PutUvarint(canonicalPrefix[:], length)
		if prefixSize != canonicalSize || !bytes.Equal(data[:prefixSize], canonicalPrefix[:canonicalSize]) {
			if !errors.Is(err, frame.ErrInvalidLength) {
				t.Fatalf("non-canonical prefix %x error = %v, want ErrInvalidLength", data[:prefixSize], err)
			}
			return
		}
		if length > fuzzUVarintPayloadLimit {
			if !errors.Is(err, frame.ErrFrameTooLarge) {
				t.Fatalf("length %d error = %v, want ErrFrameTooLarge", length, err)
			}
			return
		}
		if uint64(len(data)-prefixSize) < length {
			if !errors.Is(err, frame.ErrTruncatedFrame) {
				t.Fatalf("truncated length %d error = %v, want ErrTruncatedFrame", length, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("canonical complete frame error = %v", err)
		}
		want := data[prefixSize : prefixSize+int(length)]
		if !bytes.Equal(payload, want) || reader.Len() != len(data)-prefixSize-int(length) {
			t.Fatalf("decoded payload=%x remaining=%d, want payload=%x remaining=%d", payload, reader.Len(), want, len(data)-prefixSize-int(length))
		}
	})
}

func FuzzFrameDecoder(f *testing.F) {
	seeds := [][]byte{
		{}, {0x00}, {0x01, 'x'}, {0x02, 'x'}, {0x02, 'x', 'y', 0x00},
		append([]byte{0x80, 0x20}, bytes.Repeat([]byte{'x'}, int(fuzzFrameLimit))...),
		{0x81, 0x20}, {0x80, 0x00},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > int(fuzzFrameLimit)+binary.MaxVarintLen64+64 {
			return
		}
		reader := bytes.NewReader(data)
		payload, err := frame.ReadPayload(reader, fuzzFrameLimit)
		if err != nil {
			assertPayloadReadError(t, err)
			return
		}
		if uint64(len(payload)) > fuzzFrameLimit {
			t.Fatalf("payload length = %d, limit = %d", len(payload), fuzzFrameLimit)
		}

		consumed := len(data) - reader.Len()
		var canonical bytes.Buffer
		if err := frame.WritePayload(&canonical, payload, fuzzFrameLimit); err != nil {
			t.Fatalf("WritePayload() error = %v", err)
		}
		if !bytes.Equal(data[:consumed], canonical.Bytes()) {
			t.Fatalf("consumed frame = %x, canonical = %x", data[:consumed], canonical.Bytes())
		}
		decoded, err := frame.ReadPayload(bytes.NewReader(canonical.Bytes()), fuzzFrameLimit)
		if err != nil || !bytes.Equal(decoded, payload) {
			t.Fatalf("canonical reread = (%x, %v), want (%x, nil)", decoded, err, payload)
		}
	})
}

func assertPayloadReadError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, io.EOF) || errors.Is(err, frame.ErrInvalidLength) ||
		errors.Is(err, frame.ErrTruncatedFrame) || errors.Is(err, frame.ErrFrameTooLarge) {
		return
	}
	t.Fatalf("unexpected payload read error: %v", err)
}

func assertMessageReadError(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, frame.ErrMalformedMessage) {
		return
	}
	assertPayloadReadError(t, err)
}
