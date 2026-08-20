package security

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSignedPrincipalProviderRejectsMissingAndForgedIdentity(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	provider := SignedHeaderPrincipalProvider{Key: key}
	metadata := TransportMetadata{Protocol: "http", SessionID: "mcp-session-a", Headers: make(http.Header)}
	if _, err := provider.Authenticate(context.Background(), metadata); err == nil {
		t.Fatal("missing identity unexpectedly authenticated")
	}
	credential, err := SignedCredential(key, "principal-a", metadata.SessionID, "nonce-a", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	metadata.Headers.Set("Authorization", "Bearer "+credential)
	principal, err := provider.Authenticate(context.Background(), metadata)
	if err != nil || principal.ID != "principal-a" || principal.TransportID != metadata.SessionID {
		t.Fatalf("Authenticate() = %#v, %v", principal, err)
	}
	encodedPrincipalA := base64.RawURLEncoding.EncodeToString([]byte("principal-a"))
	encodedPrincipalB := base64.RawURLEncoding.EncodeToString([]byte("principal-b"))
	forged := strings.Replace(credential, encodedPrincipalA, encodedPrincipalB, 1)
	metadata.Headers.Set("Authorization", "Bearer "+forged)
	if _, err := provider.Authenticate(context.Background(), metadata); err == nil {
		t.Fatal("forged principal unexpectedly authenticated")
	}
	metadata.Headers.Set("Authorization", "Bearer "+credential)
	metadata.SessionID = "other-session"
	if _, err := provider.Authenticate(context.Background(), metadata); err == nil {
		t.Fatal("credential replayed on another transport session")
	}
}

func TestSignedPrincipalProviderRejectsExpiredCredential(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	credential, err := SignedCredential(key, "principal-a", "mcp-session-a", "nonce-a", time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	provider := SignedHeaderPrincipalProvider{Key: key}
	metadata := TransportMetadata{SessionID: "mcp-session-a", Headers: http.Header{"Authorization": []string{"Bearer " + credential}}}
	if _, err := provider.Authenticate(context.Background(), metadata); err == nil {
		t.Fatal("expired credential unexpectedly authenticated")
	}
}

func TestTrustedPrincipalProviderUsesPerProviderTransportContext(t *testing.T) {
	first, err := NewTrustedPrincipalProvider("principal-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewTrustedPrincipalProvider("principal-a")
	if err != nil {
		t.Fatal(err)
	}
	one, err := first.Authenticate(context.Background(), TransportMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.Authenticate(context.Background(), TransportMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID != two.ID || one.TransportID == two.TransportID || one.AuthenticationID == two.AuthenticationID {
		t.Fatalf("trusted contexts = %#v and %#v; want distinct transport bindings", one, two)
	}
}
