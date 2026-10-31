package api

import (
	"crypto"
	"errors"
	"regexp"

	"github.com/golang-jwt/jwt/v5"
)

// JWTVerifier is the passport-jwt strategy: bearer token from Authorization,
// RS256 only, exp (and nbf) enforced when present, no claim checks.
type JWTVerifier struct {
	key    crypto.PublicKey
	parser *jwt.Parser
}

func NewJWTVerifier(key crypto.PublicKey) *JWTVerifier {
	return &JWTVerifier{key: key, parser: jwt.NewParser(jwt.WithValidMethods([]string{"RS256"}))}
}

// ExtractJwt.fromAuthHeaderAsBearerToken: "<scheme> <token>", scheme
// compared case-insensitively.
var authHeader = regexp.MustCompile(`^(\S+)\s+(\S+)$`)

func (v *JWTVerifier) Verify(header string) error {
	m := authHeader.FindStringSubmatch(header)
	if m == nil || !equalFold(m[1], "bearer") {
		return errors.New("No auth token")
	}
	_, err := v.parser.Parse(m[2], func(*jwt.Token) (any, error) { return v.key, nil })
	return err
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i]|0x20, b[i]|0x20
		if x != y {
			return false
		}
	}
	return true
}
