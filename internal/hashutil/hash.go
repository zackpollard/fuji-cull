package hashutil

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
)

// SHA1File returns (hex, base64) of file's SHA-1.
func SHA1File(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), base64.StdEncoding.EncodeToString(sum), nil
}
