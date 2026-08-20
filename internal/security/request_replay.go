package security

import (
	"errors"
	"sync"
	"time"
)

const (
	MCPRequestIDMetaKey       = "repoworker/request_id"
	MCPRequestSequenceMetaKey = "repoworker/request_sequence"
	defaultReplayEntries      = 2048
	defaultReplayTTL          = 30 * time.Minute
)

var ErrRequestMetadata = errors.New("request metadata rejected")

// RequestReplayCache is a bounded, process-local replay cache for mutating
// MCP calls. The cache is deliberately keyed by both the authenticated
// control-plane session and the SDK session handle: a request identity cannot
// move between principals, transports, or sessions.
type RequestReplayCache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	entries map[replayEntryKey]replayEntry
}

type replayEntryKey struct {
	mcpSession string
	requestID  string
	sequence   uint64
}

type replayEntry struct {
	key           replayEntryKey
	transportID   string
	authSessionID string
	principalID   string
	seenAt        time.Time
}

func NewRequestReplayCache(maxEntries int, ttl time.Duration) (*RequestReplayCache, error) {
	if maxEntries <= 0 {
		maxEntries = defaultReplayEntries
	}
	if ttl <= 0 {
		ttl = defaultReplayTTL
	}
	if maxEntries > 1<<16 || ttl > 24*time.Hour {
		return nil, ErrDenied
	}
	return &RequestReplayCache{max: maxEntries, ttl: ttl, now: time.Now, entries: make(map[replayEntryKey]replayEntry)}, nil
}

func NewDefaultRequestReplayCache() *RequestReplayCache {
	cache, _ := NewRequestReplayCache(defaultReplayEntries, defaultReplayTTL)
	return cache
}

// Accept records one mutating request. Request IDs and sequences are supplied
// through the MCP SDK's actual CallToolParams._meta field; they are not tool
// arguments or invented HTTP headers. Reusing either identity in one bound
// session is rejected.
func (c *RequestReplayCache) Accept(transportID, authSessionID, principalID, mcpSessionID, requestID string, sequence uint64) error {
	if c == nil || !validOpaque(transportID) || !validOpaque(authSessionID) || !validID(principalID) || !validOpaque(mcpSessionID) || !validOpaque(requestID) || sequence == 0 {
		return ErrRequestMetadata
	}
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if !entry.seenAt.Add(c.ttl).After(now) {
			delete(c.entries, key)
		}
	}
	for _, entry := range c.entries {
		if entry.key.requestID == requestID || (entry.key.mcpSession == mcpSessionID && entry.key.sequence == sequence) {
			// Request IDs are single-use across the cache. Sequence numbers are
			// scoped to the SDK MCP session, so a fresh session may start at 1,
			// but moving an existing request ID to another session fails closed.
			return ErrReplay
		}
	}
	if len(c.entries) >= c.max {
		var oldestKey replayEntryKey
		var oldest time.Time
		for key, entry := range c.entries {
			if oldest.IsZero() || entry.seenAt.Before(oldest) {
				oldestKey = key
				oldest = entry.seenAt
			}
		}
		delete(c.entries, oldestKey)
	}
	key := replayEntryKey{mcpSession: mcpSessionID, requestID: requestID, sequence: sequence}
	c.entries[key] = replayEntry{key: key, transportID: transportID, authSessionID: authSessionID, principalID: principalID, seenAt: now}
	return nil
}
