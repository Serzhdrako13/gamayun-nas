package catalog

import (
	"crypto/ed25519"
	"embed"
	"encoding/base64"
	"fmt"
	"strings"
)

//go:embed keys
var keysFS embed.FS

// BuiltinKeys — ключи из internal/catalog/keys/*.pub.
func BuiltinKeys() ([]ed25519.PublicKey, error) {
	l, err := keysFS.ReadDir("keys")
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for _, e := range l {
		if !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		b, _ := keysFS.ReadFile("keys/" + e.Name())
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("встроенный ключ %s неверен", e.Name())
		}
		keys = append(keys, ed25519.PublicKey(k))
	}
	return keys, nil
}
