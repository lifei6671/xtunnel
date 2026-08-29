package managementapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	defaultPageSize          = 50
	maximumPageSize          = 200
	pageTokenVersion         = 1
	pageTokenMACSize         = sha256.Size
	pageTokenSortIDAscending = "id_asc"
	pageTokenDomain          = "xtunnel-management-page-v1\x00"
)

var (
	errInvalidPageToken  = errors.New("invalid page token")
	errInvalidPageSource = errors.New("invalid page source")
)

type pageTokenCodec struct {
	key [sha256.Size]byte
}

type pageTokenScope struct {
	resource string
	idPrefix string
	filter   string
}

type pageTokenPayload struct {
	Version    int    `json:"v"`
	Resource   string `json:"r"`
	Sort       string `json:"s"`
	LastID     string `json:"i"`
	FilterHash string `json:"f"`
}

// newPageTokenCodec 为当前 Management 进程创建独立签名密钥。Cursor 在进程重启后
// 自然失效，客户端只能把完整 Token 原样回传，不能依赖或伪造内部字段。
func newPageTokenCodec(random io.Reader) (*pageTokenCodec, error) {
	if random == nil {
		return nil, errors.New("management page token random source is required")
	}
	codec := &pageTokenCodec{}
	if _, err := io.ReadFull(random, codec.key[:]); err != nil {
		return nil, fmt.Errorf("generate management page token key: %w", err)
	}
	return codec, nil
}

func (codec *pageTokenCodec) encode(scope pageTokenScope, lastID string) (string, error) {
	payload, err := json.Marshal(pageTokenPayload{
		Version: pageTokenVersion, Resource: scope.resource, Sort: pageTokenSortIDAscending,
		LastID: lastID, FilterHash: pageFilterHash(scope.filter),
	})
	if err != nil {
		return "", fmt.Errorf("marshal management page token: %w", err)
	}
	mac := hmac.New(sha256.New, codec.key[:])
	if _, err := mac.Write([]byte(pageTokenDomain)); err != nil {
		return "", fmt.Errorf("sign management page token domain: %w", err)
	}
	if _, err := mac.Write(payload); err != nil {
		return "", fmt.Errorf("sign management page token: %w", err)
	}
	signed := append(payload, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(signed), nil
}

func (codec *pageTokenCodec) decode(token string, scope pageTokenScope) (pageTokenPayload, error) {
	if codec == nil || len(token) == 0 || len(token) > 4096 {
		return pageTokenPayload{}, errInvalidPageToken
	}
	signed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(signed) <= pageTokenMACSize || base64.RawURLEncoding.EncodeToString(signed) != token {
		return pageTokenPayload{}, errInvalidPageToken
	}
	payloadBytes := signed[:len(signed)-pageTokenMACSize]
	providedMAC := signed[len(signed)-pageTokenMACSize:]
	mac := hmac.New(sha256.New, codec.key[:])
	if _, err := mac.Write([]byte(pageTokenDomain)); err != nil {
		return pageTokenPayload{}, fmt.Errorf("verify management page token domain: %w", err)
	}
	if _, err := mac.Write(payloadBytes); err != nil {
		return pageTokenPayload{}, fmt.Errorf("verify management page token: %w", err)
	}
	if !hmac.Equal(providedMAC, mac.Sum(nil)) {
		return pageTokenPayload{}, errInvalidPageToken
	}
	var payload pageTokenPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil ||
		payload.Version != pageTokenVersion || payload.Resource != scope.resource ||
		payload.Sort != pageTokenSortIDAscending || payload.FilterHash != pageFilterHash(scope.filter) ||
		!validate.ValidID(payload.LastID, scope.idPrefix) {
		return pageTokenPayload{}, errInvalidPageToken
	}
	return payload, nil
}

// paginateManagementItems 在稳定的 ID 升序结果上应用签名 Cursor。它在生成下一页
// Token 前验证所有 ID 与排序不变量，避免 Repository 或 Runtime 投影顺序漂移后静默
// 产生重复、跳页或可伪造边界。
func paginateManagementItems[T any](
	codec *pageTokenCodec,
	items []T,
	pageSize *PageSize,
	pageToken *PageToken,
	scope pageTokenScope,
	idOf func(T) string,
) ([]T, *string, error) {
	size := defaultPageSize
	if pageSize != nil {
		size = *pageSize
	}
	if codec == nil || size < 1 || size > maximumPageSize || scope.resource == "" || scope.idPrefix == "" || idOf == nil {
		return nil, nil, errInvalidPageSource
	}
	for index, item := range items {
		id := idOf(item)
		if !validate.ValidID(id, scope.idPrefix) || index > 0 && idOf(items[index-1]) >= id {
			return nil, nil, errInvalidPageSource
		}
	}

	start := 0
	if pageToken != nil {
		payload, err := codec.decode(*pageToken, scope)
		if err != nil {
			return nil, nil, err
		}
		start = sort.Search(len(items), func(index int) bool {
			return idOf(items[index]) > payload.LastID
		})
	}
	end := min(start+size, len(items))
	page := items[start:end]
	if end == len(items) {
		return page, nil, nil
	}
	next, err := codec.encode(scope, idOf(page[len(page)-1]))
	if err != nil {
		return nil, nil, err
	}
	return page, &next, nil
}

func pageFilter(parts ...string) string {
	return strings.Join(parts, "\x00")
}

func pageFilterHash(filter string) string {
	hash := sha256.Sum256([]byte(filter))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func paginationFailure(err error) managementFailure {
	if errors.Is(err, errInvalidPageToken) {
		return managementFailure{status: 400, code: APIErrorCodeINVALIDPAGETOKEN, message: "page_token 无效"}
	}
	return managementFailure{status: 500, code: APIErrorCodeINTERNALERROR, message: "服务器内部错误"}
}
