package room

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

func TestHashPasswordEnforcesBcryptByteBoundary(t *testing.T) {
	if _, err := HashPassword(strings.Repeat("p", proto.MaxPasswordBytes)); err != nil {
		t.Fatalf("72-byte password rejected: %v", err)
	}
	if _, err := HashPassword(strings.Repeat("p", proto.MaxPasswordBytes+1)); !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("73-byte password err=%v", err)
	}
	// The boundary is bytes, not runes: 25 three-byte Chinese runes exceed it.
	if _, err := HashPassword(strings.Repeat("密", 25)); !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("75-byte UTF-8 password err=%v", err)
	}
}
