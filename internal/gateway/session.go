package gateway

import (
	"net/http"
	"strings"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Which task a request belongs to.
//
// One instruction to a coding agent becomes thirty or forty calls, and what
// people want to know (what a task cost, why it took so long) is about all of
// them together. So each row carries a session key.
//
// The key is a hash. It cannot be read back or searched, and says only that
// two requests belong together.

// SessionHeader is the header a client can name its own session in. The
// value is hashed, so it is never stored as sent.
//
// Deriving the session is a guess; the header is a fact. It groups a client
// exactly, even when it rewrites its history or restarts a task with the same
// words.
const SessionHeader = "X-Keera-Session"

// sessionHeaders is every header read as a session, in precedence order.
//
// There is no standard: neither the OpenAI nor the Anthropic API has a field
// for the conversation, and grouping by user would merge all of a developer's
// tasks. So the three headers used in practice are read as one: ours, the
// generic X-Session-Id, and Helicone-Session-Id.
var sessionHeaders = []string{SessionHeader, "X-Session-Id", "Helicone-Session-Id"}

// maxSessionInput bounds how much of the opening prompt is hashed. The store
// owns the derivation, so it owns the constant.
const maxSessionInput = store.MaxSessionInput

// sessionKey derives the session key for one request, or returns empty for a
// request that is not part of a conversation.
//
// Only chat gets one. Completions and embeddings have no conversation, and
// labelling them would fill reports with one-request tasks.
//
// The API key's id is part of the hash, so two developers who start with the
// same sentence are two sessions.
//
// Two limits: rewriting the opening prompt mid-task starts a new session, and
// two tasks opened with the same sentence within the idle gap merge. Both are
// why SessionHeader exists.
func sessionKey(r *http.Request, keyID string, b *body, kind policy.Kind) string {
	if kind != policy.KindChat {
		return ""
	}
	// The header's name is not hashed, so switching headers mid-task does not
	// split it.
	if stated := statedSession(r); stated != "" {
		return store.StatedSessionKeyFor(keyID, stated)
	}
	opening, ok := b.firstUserMessage()
	if !ok {
		return ""
	}
	return store.DerivedSessionKey(hashSession(keyID, "opening", opening))
}

// statedSession is the session the client named, or empty if it named none.
// The first header with a value wins, so ours beats a proxy's.
func statedSession(r *http.Request) string {
	for _, name := range sessionHeaders {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// hashSession hashes one derivation to the short string that goes on the row.
// It lives in the store because sandboxes compute the same key, and two
// copies would silently drift apart. See store.SessionHash.
func hashSession(keyID, kind string, payload []byte) string {
	return store.SessionHash(keyID, kind, payload)
}
