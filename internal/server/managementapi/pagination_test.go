package managementapi

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/lifei6671/xtunnel/internal/identity"
)

func TestManagementPaginationDefaultMaximumAndContinuation(t *testing.T) {
	codec, err := newPageTokenCodec(bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatalf("newPageTokenCodec() error = %v", err)
	}
	items := make([]string, 201)
	for index := range items {
		items[index], err = identity.NewTunnelID()
		if err != nil {
			t.Fatalf("NewTunnelID() error = %v", err)
		}
	}
	slices.Sort(items)
	scope := pageTokenScope{resource: "tunnels", idPrefix: "tun_", filter: pageFilter("status", "")}

	first, next, err := paginateManagementItems(codec, items, nil, nil, scope, func(id string) string { return id })
	if err != nil || len(first) != defaultPageSize || next == nil {
		t.Fatalf("default page = %d items, next %v, error %v", len(first), next != nil, err)
	}
	maximum := PageSize(maximumPageSize)
	second, final, err := paginateManagementItems(codec, items, &maximum, (*PageToken)(next), scope, func(id string) string { return id })
	if err != nil || len(second) != 151 || final != nil {
		t.Fatalf("continuation page = %d items, next %v, error %v", len(second), final != nil, err)
	}
	combined := append(slices.Clone(first), second...)
	if !slices.Equal(combined, items) {
		t.Fatal("pagination returned duplicate, missing, or reordered Tunnel IDs")
	}

	firstMaximum, maximumNext, err := paginateManagementItems(codec, items, &maximum, nil, scope, func(id string) string { return id })
	if err != nil || len(firstMaximum) != maximumPageSize || maximumNext == nil {
		t.Fatalf("maximum page = %d items, next %v, error %v", len(firstMaximum), maximumNext != nil, err)
	}
}

func TestManagementPageTokenRejectsTamperingAndScopeChanges(t *testing.T) {
	codec, err := newPageTokenCodec(bytes.NewReader(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatalf("newPageTokenCodec() error = %v", err)
	}
	otherCodec, err := newPageTokenCodec(bytes.NewReader(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatalf("newPageTokenCodec(other) error = %v", err)
	}
	scope := pageTokenScope{resource: "tunnels", idPrefix: "tun_", filter: pageFilter("status", "ONLINE")}
	token, err := codec.encode(scope, "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	if _, err := codec.decode(token, scope); err != nil {
		t.Fatalf("decode() error = %v", err)
	}

	replacement := byte('A')
	if token[len(token)-1] == replacement {
		replacement = 'B'
	}
	tampered := token[:len(token)-1] + string(replacement)
	tests := []struct {
		name  string
		codec *pageTokenCodec
		token string
		scope pageTokenScope
	}{
		{name: "tampered", codec: codec, token: tampered, scope: scope},
		{name: "different process key", codec: otherCodec, token: token, scope: scope},
		{name: "different resource", codec: codec, token: token, scope: pageTokenScope{resource: "services", idPrefix: "svc_", filter: scope.filter}},
		{name: "different filter", codec: codec, token: token, scope: pageTokenScope{resource: scope.resource, idPrefix: scope.idPrefix, filter: pageFilter("status", "OFFLINE")}},
		{name: "empty", codec: codec, token: "", scope: scope},
		{name: "non canonical base64", codec: codec, token: token + "=", scope: scope},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.codec.decode(test.token, test.scope); !errors.Is(err, errInvalidPageToken) {
				t.Fatalf("decode() error = %v, want errInvalidPageToken", err)
			}
		})
	}
}

func TestManagementPaginationRejectsInvalidSourceAndRandomFailure(t *testing.T) {
	if _, err := newPageTokenCodec(errorReader{}); err == nil {
		t.Fatal("newPageTokenCodec() accepted a failing random source")
	}
	codec, err := newPageTokenCodec(bytes.NewReader(bytes.Repeat([]byte{4}, 32)))
	if err != nil {
		t.Fatalf("newPageTokenCodec() error = %v", err)
	}
	duplicate := []string{"tun_01ARZ3NDEKTSV4RRFFQ69G5FAV", "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV"}
	if _, _, err := paginateManagementItems(
		codec, duplicate, nil, nil,
		pageTokenScope{resource: "tunnels", idPrefix: "tun_"}, func(id string) string { return id },
	); !errors.Is(err, errInvalidPageSource) {
		t.Fatalf("paginateManagementItems() error = %v, want errInvalidPageSource", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
