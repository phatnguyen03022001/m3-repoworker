// Package operator implements the private operator-only confirmation channel.
// It is intentionally not imported by the autonomous MCP adapter.
package operator

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tienphat/m3-repoworker/internal/security"
)

const (
	maxFrameBytes   = 16 << 10
	maxReplayNonces = 4096
	replayTTL       = time.Hour
)

var ErrRejected = errors.New("operator request rejected")

type ApprovalFunc func(context.Context, security.OperatorConfirmationRequest) (security.Confirmation, error)

type wireRequest struct {
	OperatorID string                       `json:"operator_id"`
	Nonce      string                       `json:"nonce"`
	Class      security.ConfirmationClass   `json:"class"`
	TTLSeconds int64                        `json:"ttl_seconds"`
	Binding    security.ConfirmationBinding `json:"binding"`
	Signature  string                       `json:"signature"`
}

type wireResponse struct {
	Confirmation *security.Confirmation `json:"confirmation,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

type signaturePayload struct {
	OperatorID        string                     `json:"operator_id"`
	Nonce             string                     `json:"nonce"`
	Class             security.ConfirmationClass `json:"class"`
	TTLSeconds        int64                      `json:"ttl_seconds"`
	Action            string                     `json:"action"`
	RepositoryID      string                     `json:"repository_id"`
	PrincipalID       string                     `json:"principal_id"`
	SessionID         string                     `json:"session_id"`
	GenerationID      string                     `json:"generation_id"`
	FencingGeneration uint64                     `json:"fencing_generation"`
	CandidateSnapshot string                     `json:"candidate_snapshot"`
	PlanDigest        string                     `json:"plan_digest"`
}

type Server struct {
	path      string
	key       []byte
	operator  security.AuthenticatedOperatorAuthority
	approve   ApprovalFunc
	listener  *net.UnixListener
	closeOnce sync.Once
	wg        sync.WaitGroup

	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func NewServer(path string, key []byte, operatorID string, approve ApprovalFunc) (*Server, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(key) < 32 || approve == nil {
		return nil, ErrRejected
	}
	authority, err := security.NewAuthenticatedOperatorAuthority(operatorID)
	if err != nil {
		return nil, ErrRejected
	}
	return &Server{path: path, key: append([]byte(nil), key...), operator: authority, approve: approve, seen: make(map[string]time.Time), now: time.Now}, nil
}

func (s *Server) Start() error {
	if s == nil || s.path == "" || s.approve == nil {
		return ErrRejected
	}
	parent, err := os.Stat(filepath.Dir(s.path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return ErrRejected
	}
	if existing, err := os.Lstat(s.path); err == nil {
		if existing.Mode()&os.ModeSocket == 0 {
			return ErrRejected
		}
		if conn, dialErr := net.DialTimeout("unix", s.path, 100*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			return ErrRejected
		}
		if err := os.Remove(s.path); err != nil {
			return ErrRejected
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrRejected
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		return ErrRejected
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.path)
		return ErrRejected
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		_ = listener.Close()
		_ = os.Remove(s.path)
		return ErrRejected
	}
	s.listener = listener
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	frame, err := readBoundedFrame(bufio.NewReaderSize(conn, 1024))
	if err != nil {
		writeResponse(conn, wireResponse{Error: "operator request rejected"})
		return
	}
	var request wireRequest
	decoder := json.NewDecoder(strings.NewReader(string(frame)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || !s.verify(request) {
		writeResponse(conn, wireResponse{Error: "operator request rejected"})
		return
	}
	ctx := security.WithOperatorAuthentication(context.Background(), request.OperatorID)
	confirmation, err := s.approve(ctx, security.OperatorConfirmationRequest{Binding: request.Binding, Class: request.Class, TTL: time.Duration(request.TTLSeconds) * time.Second})
	if err != nil {
		writeResponse(conn, wireResponse{Error: "operator request rejected"})
		return
	}
	writeResponse(conn, wireResponse{Confirmation: &confirmation})
}

func (s *Server) verify(request wireRequest) bool {
	if s == nil || request.OperatorID != s.operator.OperatorID || !validValue(request.Nonce) || request.TTLSeconds <= 0 || request.TTLSeconds > 3600 || (request.Class != security.ConfirmationDestructive && request.Class != security.ConfirmationPublication) || !validValue(request.Signature) {
		return false
	}
	payload, err := json.Marshal(signaturePayloadFor(request))
	if err != nil {
		return false
	}
	signature, err := hex.DecodeString(request.Signature)
	if err != nil || len(signature) != sha256.Size {
		return false
	}
	hash := hmac.New(sha256.New, s.key)
	_, _ = hash.Write(payload)
	if !hmac.Equal(signature, hash.Sum(nil)) {
		return false
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for nonce, seenAt := range s.seen {
		if !seenAt.Add(replayTTL).After(now) {
			delete(s.seen, nonce)
		}
	}
	if _, exists := s.seen[request.Nonce]; exists {
		return false
	}
	if len(s.seen) >= maxReplayNonces {
		var oldestNonce string
		var oldest time.Time
		for nonce, seenAt := range s.seen {
			if oldest.IsZero() || seenAt.Before(oldest) {
				oldestNonce, oldest = nonce, seenAt
			}
		}
		delete(s.seen, oldestNonce)
	}
	s.seen[request.Nonce] = now
	return true
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.path)
	})
	s.wg.Wait()
	return nil
}

func Approve(ctx context.Context, socketPath string, key []byte, operatorID string, request security.OperatorConfirmationRequest) (security.Confirmation, error) {
	if ctx == nil || !filepath.IsAbs(socketPath) || len(key) < 32 || !validValue(operatorID) || request.Binding.PrincipalID == operatorID {
		return security.Confirmation{}, ErrRejected
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return security.Confirmation{}, ErrRejected
	}
	nonce := "operator_" + hex.EncodeToString(nonceBytes)
	requestWire := wireRequest{OperatorID: operatorID, Nonce: nonce, Class: request.Class, TTLSeconds: int64(request.TTL / time.Second), Binding: request.Binding}
	if requestWire.TTLSeconds <= 0 {
		return security.Confirmation{}, ErrRejected
	}
	payload, err := json.Marshal(signaturePayloadFor(requestWire))
	if err != nil {
		return security.Confirmation{}, ErrRejected
	}
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write(payload)
	requestWire.Signature = hex.EncodeToString(hash.Sum(nil))
	frame, err := json.Marshal(requestWire)
	if err != nil || len(frame)+1 > maxFrameBytes {
		return security.Confirmation{}, ErrRejected
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return security.Confirmation{}, ErrRejected
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(append(frame, '\n')); err != nil {
		return security.Confirmation{}, ErrRejected
	}
	responseFrame, err := readBoundedFrame(bufio.NewReaderSize(conn, 1024))
	if err != nil {
		return security.Confirmation{}, ErrRejected
	}
	var response wireResponse
	if json.Unmarshal(responseFrame, &response) != nil || response.Confirmation == nil || response.Error != "" {
		return security.Confirmation{}, ErrRejected
	}
	return *response.Confirmation, nil
}

func signaturePayloadFor(request wireRequest) signaturePayload {
	return signaturePayload{OperatorID: request.OperatorID, Nonce: request.Nonce, Class: request.Class, TTLSeconds: request.TTLSeconds, Action: request.Binding.Action, RepositoryID: request.Binding.RepositoryID, PrincipalID: request.Binding.PrincipalID, SessionID: request.Binding.SessionID, GenerationID: request.Binding.GenerationID, FencingGeneration: request.Binding.FencingGeneration, CandidateSnapshot: request.Binding.CandidateSnapshot, PlanDigest: request.Binding.PlanDigest}
}

func writeResponse(conn *net.UnixConn, response wireResponse) {
	frame, err := json.Marshal(response)
	if err == nil && len(frame)+1 <= maxFrameBytes {
		_, _ = conn.Write(append(frame, '\n'))
	}
}

func readBoundedFrame(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, ErrRejected
	}
	frame := make([]byte, 0, 1024)
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > maxFrameBytes-len(frame) {
			return nil, ErrRejected
		}
		frame = append(frame, part...)
		if err == nil {
			if len(frame) == 0 {
				return nil, ErrRejected
			}
			return frame, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, ErrRejected
		}
	}
}

func validValue(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}
