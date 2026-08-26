package application

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const aes256KeyBytes = 32

var (
	// ErrTokenProtectorKey 表示主密钥不是 AES-256 要求的固定 32 字节。
	ErrTokenProtectorKey = errors.New("connection token protector key is invalid")
	// ErrTokenProtection 表示 Token 加密或解密失败；错误不会携带明文、密文或密钥。
	ErrTokenProtection = errors.New("connection token protection failed")
)

// TokenProtectionContext 是 Token 密文不可变的数据库行身份。
// AES-GCM 将它作为 AAD 认证，密文即使被复制到另一 Tunnel、Token 或版本也无法解密。
type TokenProtectionContext struct {
	TunnelID string
	TokenID  string
	Version  int64
}

// TokenProtector 隔离 Connection Token 的可逆保护能力。
// 实现不得记录传入的明文、密文或主密钥。
type TokenProtector interface {
	Seal([]byte, TokenProtectionContext) ([]byte, error)
	Open([]byte, TokenProtectionContext) ([]byte, error)
}

// AES256GCMTokenProtector 使用 AES-256-GCM 保存可重复获取的完整 Connection Token。
type AES256GCMTokenProtector struct {
	aead   cipher.AEAD
	random io.Reader
}

// NewAES256GCMTokenProtector 使用调用方注入的 32 字节主密钥创建保护器。
// 本构造器只复制密钥并建立 cipher；密钥文件的生成、权限和生命周期由 Bootstrap 负责。
func NewAES256GCMTokenProtector(key []byte) (*AES256GCMTokenProtector, error) {
	return newAES256GCMTokenProtector(key, rand.Reader)
}

func newAES256GCMTokenProtector(key []byte, random io.Reader) (*AES256GCMTokenProtector, error) {
	if len(key) != aes256KeyBytes || random == nil {
		return nil, ErrTokenProtectorKey
	}
	block, err := aes.NewCipher(append([]byte(nil), key...))
	if err != nil {
		return nil, ErrTokenProtectorKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrTokenProtectorKey
	}
	return &AES256GCMTokenProtector{aead: aead, random: random}, nil
}

// Seal 使用全新随机 nonce 加密 Token，并把 nonce 放在密文前部供 Open 读取。
func (protector *AES256GCMTokenProtector) Seal(plaintext []byte, context TokenProtectionContext) ([]byte, error) {
	if protector == nil || protector.aead == nil || protector.random == nil || len(plaintext) == 0 {
		return nil, ErrTokenProtection
	}
	nonce := make([]byte, protector.aead.NonceSize())
	if _, err := io.ReadFull(protector.random, nonce); err != nil {
		return nil, fmt.Errorf("%w: generate nonce", ErrTokenProtection)
	}
	sealed := make([]byte, len(nonce), len(nonce)+len(plaintext)+protector.aead.Overhead())
	copy(sealed, nonce)
	sealed = protector.aead.Seal(sealed, nonce, plaintext, protectionAAD(context))
	return sealed, nil
}

// Open 校验密文及其绑定行身份后返回 Token 明文。
func (protector *AES256GCMTokenProtector) Open(ciphertext []byte, context TokenProtectionContext) ([]byte, error) {
	if protector == nil || protector.aead == nil || len(ciphertext) <= protector.aead.NonceSize()+protector.aead.Overhead() {
		return nil, ErrTokenProtection
	}
	nonceSize := protector.aead.NonceSize()
	plaintext, err := protector.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], protectionAAD(context))
	if err != nil {
		return nil, ErrTokenProtection
	}
	return plaintext, nil
}

func protectionAAD(context TokenProtectionContext) []byte {
	// 长度前缀让拼接保持无歧义；固定域分隔同一主密钥未来可能保护的其他数据类型。
	const domain = "xtunnel-connection-token-at-rest-v1"
	result := make([]byte, 0, len(domain)+4+len(context.TunnelID)+4+len(context.TokenID)+8)
	result = append(result, domain...)
	result = binary.BigEndian.AppendUint32(result, uint32(len(context.TunnelID)))
	result = append(result, context.TunnelID...)
	result = binary.BigEndian.AppendUint32(result, uint32(len(context.TokenID)))
	result = append(result, context.TokenID...)
	result = binary.BigEndian.AppendUint64(result, uint64(context.Version))
	return result
}
