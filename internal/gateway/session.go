package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Which task a request belongs to. One instruction to a coding agent becomes
// dozens of calls, and people want to know what the whole task cost, so each
// row carries a session key. It is a hash, which only says that two requests
// belong together.

// sessionHeaders is every header read as a session, in precedence order.
// There is no standard, so the ones in use are read as one: ours, the generic
// X-Session-Id, Helicone-Session-Id, and Claude Code's own, which it sends on
// every call, subagents included, and keeps across compaction.
var sessionHeaders = []string{
	httpx.SessionHeader, "X-Session-Id", "Helicone-Session-Id",
	"X-Claude-Code-Session-Id",
}

// sessionKey derives the session key for one request, or empty outside a
// conversation. Only chat gets one, or reports would fill with one-request
// tasks. The API key's id is hashed in, so two developers who start with the
// same sentence are two sessions. A rewritten opening prompt starts a new
// session, and two tasks opened with the same sentence within the idle gap
// merge; httpx.SessionHeader fixes both.
func sessionKey(r *http.Request, keyID string, kind policy.Kind,
	opening func() (json.RawMessage, bool),
) string {
	if kind != policy.KindChat {
		return ""
	}
	// The header's name is not hashed, so switching headers keeps the task.
	if stated := statedSession(r); stated != "" {
		return store.StatedSessionKeyFor(keyID, stated)
	}
	first, ok := opening()
	if !ok {
		return ""
	}
	// The hash lives in the store, next to the stated one sandboxes use.
	return store.DerivedSessionKey(store.SessionHash(keyID, store.SessionKindOpening, first))
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
