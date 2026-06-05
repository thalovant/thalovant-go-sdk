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
