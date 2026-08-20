package security

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var ErrUnauthenticated = errors.New("authentication required")

// TransportMetadata is supplied by the transport adapter, never by a tool
// argument. HTTP adapters populate Headers and SessionID; a trusted local
// adapter may populate TrustedName from explicit process configuration.
type TransportMetadata struct {
	Protocol    string
	SessionID   string
	Headers     http.Header
	TrustedName string
}

type Principal struct {
	ID               string
	TransportID      string
	AuthenticationID string
	ExpiresAt        time.Time
}

// PrincipalProvider is the authentication boundary for the control plane.
// Implementations must validate transport credentials before returning a
// principal. The control plane never accepts a principal from tool input.
type PrincipalProvider interface {
	Authenticate(context.Context, TransportMetadata) (Principal, error)
}

// TrustedPrincipalProvider is an explicit configuration for a transport that
// is already authenticated outside RepoWorker (for example a private local
// connector process). It is never a production default: Config must provide
// the provider explicitly.
type TrustedPrincipalProvider struct {
	PrincipalID string
	TransportID string
}

func NewTrustedPrincipalProvider(principalID string) (TrustedPrincipalProvider, error) {
	if !validID(principalID) {
		return TrustedPrincipalProvider{}, ErrUnauthenticated
	}
	transportID, err := newOpaque("transport_")
	if err != nil {
		return TrustedPrincipalProvider{}, ErrUnauthenticated
	}
	return TrustedPrincipalProvider{PrincipalID: principalID, TransportID: transportID}, nil
}

func (p TrustedPrincipalProvider) Authenticate(ctx context.Context, metadata TransportMetadata) (Principal, error) {
	if ctx == nil || !validID(p.PrincipalID) {
		return Principal{}, ErrUnauthenticated
	}
	transportID := metadata.SessionID
	if transportID == "" {
		transportID = metadata.TrustedName
	}
	if transportID == "" {
		transportID = p.TransportID
	}
	if !validOpaque(transportID) {
		return Principal{}, ErrUnauthenticated
	}
	authenticationID := digestString("trusted\x00" + p.PrincipalID + "\x00" + transportID)
	return Principal{ID: p.PrincipalID, TransportID: transportID, AuthenticationID: authenticationID, ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}, nil
}

// SignedHeaderPrincipalProvider validates a short-lived HMAC credential in an
// HTTP header. The credential binds the principal and MCP transport session;
// the raw credential is never returned, logged, or persisted.
type SignedHeaderPrincipalProvider struct {
	Key        []byte
	HeaderName string
	Now        func() time.Time
}

func (p SignedHeaderPrincipalProvider) Authenticate(ctx context.Context, metadata TransportMetadata) (Principal, error) {
	if ctx == nil || len(p.Key) < 32 || metadata.SessionID == "" {
		return Principal{}, ErrUnauthenticated
	}
	headerName := p.HeaderName
	if headerName == "" {
		headerName = "Authorization"
	}
	credential := metadata.Headers.Get(headerName)
	if headerName == "Authorization" {
		const prefix = "Bearer "
		if !strings.HasPrefix(credential, prefix) {
			return Principal{}, ErrUnauthenticated
		}
		credential = strings.TrimSpace(strings.TrimPrefix(credential, prefix))
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 6 || parts[0] != "rw1" {
		return Principal{}, ErrUnauthenticated
	}
	principalID, err := decodeCredentialPart(parts[1])
	if err != nil || !validID(principalID) {
		return Principal{}, ErrUnauthenticated
	}
	transportID, err := decodeCredentialPart(parts[2])
	if err != nil || transportID != metadata.SessionID || !validOpaque(transportID) {
		return Principal{}, ErrUnauthenticated
	}
	expiresUnix, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || expiresUnix <= 0 {
		return Principal{}, ErrUnauthenticated
	}
	nonce, err := decodeCredentialPart(parts[4])
	if err != nil || !validOpaque(nonce) {
		return Principal{}, ErrUnauthenticated
	}
	signature, err := hex.DecodeString(parts[5])
	if err != nil || len(signature) != sha256.Size {
		return Principal{}, ErrUnauthenticated
	}
	message := strings.Join([]string{"rw1", principalID, transportID, parts[3], nonce}, "|")
	hash := hmac.New(sha256.New, p.Key)
	_, _ = hash.Write([]byte(message))
	if !hmac.Equal(signature, hash.Sum(nil)) {
		return Principal{}, ErrUnauthenticated
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	expiresAt := time.Unix(expiresUnix, 0).UTC()
	if !expiresAt.After(now) || expiresAt.After(now.Add(24*time.Hour)) {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{ID: principalID, TransportID: transportID, AuthenticationID: digestString("signed\x00" + credential), ExpiresAt: expiresAt}, nil
}

// SignedCredential creates a test/operator-issued credential for the signed
// header provider. Callers should transmit it only as an HTTP Authorization
// header and must not persist it in RepoWorker state.
func SignedCredential(key []byte, principalID, transportID, nonce string, expiresAt time.Time) (string, error) {
	if len(key) < 32 || !validID(principalID) || !validOpaque(transportID) || !validOpaque(nonce) || expiresAt.IsZero() {
		return "", ErrUnauthenticated
	}
	expires := strconv.FormatInt(expiresAt.UTC().Unix(), 10)
	message := strings.Join([]string{"rw1", principalID, transportID, expires, nonce}, "|")
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(message))
	encode := base64.RawURLEncoding.EncodeToString
	return strings.Join([]string{"rw1", encode([]byte(principalID)), encode([]byte(transportID)), expires, encode([]byte(nonce)), hex.EncodeToString(hash.Sum(nil))}, "."), nil
}

func decodeCredentialPart(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return "", ErrUnauthenticated
	}
	return string(decoded), nil
}
