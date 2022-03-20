package assetserver

import (
	"crypto/sha256"
	"testing"

	"github.com/tv42/zbase32"
)

func TestHashName(t *testing.T) {
	// WIP WIP WIP
}

func hash(text string) string {
	b := sha256.Sum256([]byte(text))
	return zbase32.EncodeToString(b[:hashBytes])
}
