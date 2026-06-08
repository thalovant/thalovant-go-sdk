package thalovant

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type encryptedJSON struct {
	Ciphertext string `json:"ciphertext"`
	Tag        string `json:"tag"`
	Nonce      string `json:"nonce"`
}

const binaryNonceSize = 16

func RuntimeCryptoKey(raw string) []byte {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return nil
	}
	if len(normalized) > 16 {
		normalized = normalized[:16]
	}
	return []byte(normalized)
}

func EncryptAsJSON(key string, plaintext string) (string, error) {
	runtimeKey := RuntimeCryptoKey(key)
	block, err := aes.NewCipher(runtimeKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	ciphertext := sealed[:len(sealed)-gcm.Overhead()]
	tag := sealed[len(sealed)-gcm.Overhead():]
	out, err := json.Marshal(encryptedJSON{
		Ciphertext: hex.EncodeToString(ciphertext),
		Tag:        hex.EncodeToString(tag),
		Nonce:      hex.EncodeToString(nonce),
	})
	return string(out), err
}

func DecryptFromJSON(key string, raw string) (string, error) {
	var encrypted encryptedJSON
	if err := json.Unmarshal([]byte(raw), &encrypted); err != nil {
		return "", err
	}
	runtimeKey := RuntimeCryptoKey(key)
	block, err := aes.NewCipher(runtimeKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce, err := hex.DecodeString(encrypted.Nonce)
	if err != nil {
		return "", err
	}
	ciphertext, err := hex.DecodeString(encrypted.Ciphertext)
	if err != nil {
		return "", err
	}
	tag, err := hex.DecodeString(encrypted.Tag)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("decrypt HiveMind JSON payload: %w", err)
	}
	return string(plaintext), nil
}

func EncryptAsBinary(key string, plaintext []byte) ([]byte, error) {
	runtimeKey := RuntimeCryptoKey(key)
	block, err := aes.NewCipher(runtimeKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, binaryNonceSize)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, binaryNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

func DecryptBinary(key string, payload []byte) ([]byte, error) {
	runtimeKey := RuntimeCryptoKey(key)
	block, err := aes.NewCipher(runtimeKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, binaryNonceSize)
	if err != nil {
		return nil, err
	}
	if len(payload) <= binaryNonceSize+gcm.Overhead() {
		return nil, fmt.Errorf("decrypt HiveMind binary payload: invalid payload length")
	}
	nonce := payload[:binaryNonceSize]
	ciphertext := payload[binaryNonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt HiveMind binary payload: %w", err)
	}
	return plaintext, nil
}
