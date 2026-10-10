package backup

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// HostKey is a scanned server identity, not trusted until explicitly confirmed.
type HostKey struct {
	Address     string
	Key         string
	Fingerprint string
	Known       bool
}

func ED25519Fingerprint(key string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(data) != 51 || binary.BigEndian.Uint32(data[:4]) != 11 || string(data[4:15]) != "ssh-ed25519" || binary.BigEndian.Uint32(data[15:19]) != 32 {
		return "", fmt.Errorf("invalid ED25519 host key")
	}
	sum := sha256.Sum256(data)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}
