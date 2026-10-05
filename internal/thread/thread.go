package thread

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	StatusActive = "active"
	StatusClosed = "closed"

	RoleOwner  = "owner"
	RoleMember = "member"

	MemberActive  = "active"
	MemberRemoved = "removed"

	InvitePrefix = "kf_thread_inv_"
)

var ErrInvalidInviteToken = errors.New("invalid thread invite token")

// NewInviteToken creates the one-time bearer secret returned by
// thread_invite. Only its SHA-256 digest is persisted.
func NewInviteToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	token := InvitePrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

// HashInviteToken validates the public token shape and returns the
// digest used for lookup. The raw token never reaches storage or logs.
func HashInviteToken(token string) ([]byte, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, InvitePrefix) {
		return nil, ErrInvalidInviteToken
	}
	encoded := strings.TrimPrefix(token, InvitePrefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidInviteToken
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:], nil
}
