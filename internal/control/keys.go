package control

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/id"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// keyRequest is the body of POST /v1/keys.
type keyRequest struct {
	OrgID     string         `json:"org_id"`
	ProjectID string         `json:"project_id"`
	UserID    string         `json:"user_id"`
	Name      string         `json:"name"`
	Kind      policy.KeyKind `json:"kind"`
	ExpiresIn *int64         `json:"expires_in_seconds"`
	ExpiresAt *time.Time     `json:"expires_at"`
	// Pending leaves the key for the person it is for to activate. Until
	// then it has no secret, so the administrator never sees it.
	Pending bool `json:"pending"`
}

// check fills in the defaults and refuses what is wrong with the request on
// its face. What needs the database, such as whether the person is in the
// organisation, is createKey's.
func (in *keyRequest) check() error {
	in.Kind = cmp.Or(in.Kind, policy.KeyStandard)
	if !in.Kind.Valid() {
		return errors.New("'kind' must be standard or subscription")
	}
	in.Name = cmp.Or(strings.TrimSpace(in.Name), "unnamed")
	// A subscription key goes next to one person's own Claude sign-in, so it is
	// always somebody's.
	if in.Kind == policy.KeySubscription && in.UserID == "" {
		return errors.New("a subscription key is for one person's Claude Code, so it must be " +
			"attributed to them; name them in 'user_id'")
	}
	if in.Pending && in.UserID == "" {
		return errors.New("a pending key is activated by the person it is for, so name them " +
			"in 'user_id'")
	}
	return nil
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in keyRequest
	if !readJSON(w, r, &in) {
		return
	}
	if err := in.check(); err != nil {
		badRequest(w, err.Error())
		return
	}
	orgID, ok := s.requireOrg(w, p, in.OrgID, orgRequired)
	if !ok {
		return
	}
	// Only an administrator issues keys, so each one has the project and
	// guardrails they chose for it. A key a member issued themselves would have
	// only the organisation's, and step around their project's.
	if !p.CanAdminOrg(orgID) {
		forbid(w, "only an administrator of this organisation can issue a key; "+
			"ask one for a key in your name, and rotate it yourself from then on")
		return
	}
	if !s.subscriptionsAllowed(w, in.Kind == policy.KeySubscription) {
		return
	}
	// The person a key is attributed to is who its spend is reported under,
	// so it must be someone in this organisation.
	if in.UserID != "" {
		user, err := s.st.UserByID(r.Context(), in.UserID)
		if !s.inOrg(w, orgID, user.OrgID, err) {
			return
		}
		if user.Disabled() {
			forbid(w, user.Email+" is disabled, so a key for them would not work; "+
				"enable them first")
			return
		}
	}
	// A key on another tenant's project would carry their system prompt, spend
	// their budget and use their rate limit.
	if in.ProjectID != "" && !s.requireProjectInOrg(w, r, in.ProjectID, orgID) {
		return
	}
	info := store.KeyInfo{
		ID: id.New("key"), OrgID: orgID, ProjectID: in.ProjectID, UserID: in.UserID, Name: in.Name,
		Kind: in.Kind,
	}
	if info.ExpiresAt, ok = keyExpiry(w, in.ExpiresIn, in.ExpiresAt); !ok {
		return
	}

	var secret string
	var hash []byte
	if !in.Pending {
		secret, hash, info.Prefix = auth.Generate()
	}
	info, err := s.st.CreateKey(r.Context(), info, hash)
	if errors.Is(err, store.ErrNoProject) {
		noProject(w)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// The attribution decides whose budget and report the spend lands in, so
	// it is part of the record.
	s.auditf(r, p, orgID, "key.create", "key", info.ID, map[string]any{
		"org_id": info.OrgID, "project_id": info.ProjectID, "user_id": info.UserID,
		"name": info.Name, "prefix": info.Prefix, "kind": info.Kind, "pending": info.Pending,
	})
	s.keyChanged(r, info.ID)

	// The secret is returned exactly once. Nothing stores it, so nobody can
	// read a developer's key later. A pending key has none yet.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key string `json:"key,omitempty"`
	}{KeyInfo: info, Key: secret})
}

// activateKey gives a pending key its secret and returns it, once. The holder
// may do it, and so may an administrator, who could rotate it anyway. An
// optional 'name' renames it, which is how `keera connect` names a key after
// its machine.
func (s *Server) activateKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	keyID := r.PathValue("id")
	o, ok := s.keyToManage(w, r, p, keyID, "activate")
	if !ok {
		return
	}
	if !s.subscriptionsAllowed(w, o.Kind == policy.KeySubscription) {
		return
	}
	secret, hash, prefix := auth.Generate()
	info, err := s.st.ActivateKey(r.Context(), writeOrg(p), keyID, strings.TrimSpace(in.Name), hash, prefix)
	switch {
	case errors.Is(err, policy.ErrKeyRevoked):
		conflict(w, "key_revoked",
			"that key was revoked, so it cannot be activated; ask for a new one")
		return
	case errors.Is(err, policy.ErrKeyExpired):
		conflict(w, "key_expired",
			"that key expired before it was activated; ask for a new one")
		return
	case errors.Is(err, store.ErrKeyActive):
		conflict(w, "key_active",
			"that key is already activated, and its secret was shown then; rotate it "+
				"to get a new secret")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.auditf(r, p, o.OrgID, "key.activate", "key", info.ID, map[string]any{
		"user_id": info.UserID, "name": info.Name, "prefix": info.Prefix,
	})
	s.keyChanged(r, info.ID)
	httpx.WriteJSON(w, http.StatusOK, struct {
		store.KeyInfo
		Key string `json:"key"`
	}{KeyInfo: info, Key: secret})
}

// rotateKey replaces a key with a new one that keeps its project, person, kind
// and guardrails, and revokes the old one. Whoever may revoke a key may rotate
// it, so a member can replace their own leaked key. Only an administrator may
// change its lifetime, so a 30-day key is not rotated into one for years.
func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name      string     `json:"name"`
		ExpiresIn *int64     `json:"expires_in_seconds"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	oldID := r.PathValue("id")
	o, ok := s.keyToManage(w, r, p, oldID, "rotate")
	if !ok {
		return
	}
	owner := o.OrgID
	if !s.subscriptionsAllowed(w, o.Kind == policy.KeySubscription) {
		return
	}
	if (in.ExpiresIn != nil || in.ExpiresAt != nil) && !p.CanAdminOrg(owner) {
		forbid(w, "a member cannot choose how long a key lasts; "+
			"the new key keeps the old one's lifetime")
		return
	}
	next := store.KeyInfo{ID: id.New("key"), Name: strings.TrimSpace(in.Name)}
	if next.ExpiresAt, ok = keyExpiry(w, in.ExpiresIn, in.ExpiresAt); !ok {
		return
	}
	secret, hash, prefix := auth.Generate()
	next.Prefix = prefix
	info, err := s.st.RotateKey(r.Context(), writeOrg(p), oldID, next, hash)
	if errors.Is(err, policy.ErrKeyRevoked) {
		conflict(w, "key_revoked",
			"that key was already revoked, so there is nothing to rotate; issue a new one")
		return
	}
	if errors.Is(err, policy.ErrKeyPending) {
		conflict(w, "key_pending",
			"that key is not activated yet, so it has no secret to replace; activate it instead")
		return
	}
	if errors.Is(err, store.ErrSandboxKey) {
		conflict(w, "sandbox_key",
			"that key belongs to a sandbox and ends with it, so it cannot be rotated; "+
				"resuming an expired sandbox gives it a new key")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, owner, "key.rotate", "key", info.ID, map[string]any{
		"replaced_key_id": oldID, "project_id": info.ProjectID, "user_id": info.UserID,
		"name": info.Name, "prefix": info.Prefix, "kind": info.Kind,
	})
	s.keyChanged(r, oldID)

	// The secret is returned exactly once, as on create.
	httpx.WriteJSON(w, http.StatusCreated, struct {
		store.KeyInfo
		Key           string `json:"key"`
		ReplacedKeyID string `json:"replaced_key_id"`
	}{KeyInfo: info, Key: secret, ReplacedKeyID: oldID})
}

// renameKey is open to whoever may revoke the key.
func (s *Server) renameKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		badRequest(w, "send the new 'name'; it cannot be empty")
		return
	}
	keyID := r.PathValue("id")
	o, ok := s.keyToManage(w, r, p, keyID, "rename")
	if !ok {
		return
	}
	old, err := s.st.RenameKey(r.Context(), writeOrg(p), keyID, name)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditf(r, p, o.OrgID, "key.rename", "key", keyID, map[string]any{
		"user_id": o.UserID, "from": old, "to": name,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": keyID, "name": name})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	q := r.URL.Query()
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	// By default, spend uses the same window as the projects screen, so the
	// two agree. The keys screen sends ?since from its range picker.
	since := policy.PeriodMonth.Start(time.Now())
	if q.Get("since") != "" {
		from, _, err := timeRange("", "", q.Get("since"))
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		since = from
	}
	since, _, _ = store.DayWindow(since, time.Now())
	keys, err := s.st.KeySummaries(r.Context(), store.KeyQuery{
		OrgID: orgID, ProjectID: q.Get("project_id"), Since: since,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"data": keys, "since": since,
	})
}

// keyToManage reads whose key keyID is, and checks the caller may verb it.
// Another tenant's key answers 404. Inside their organisation a member can
// already see every key, so they are told whose key it is rather than "not
// found".
func (s *Server) keyToManage(w http.ResponseWriter, r *http.Request, p *authn.Principal,
	keyID, verb string,
) (store.KeyOwner, bool) {
	o, err := s.st.KeyOwnerOf(r.Context(), keyID)
	if err != nil {
		s.fail(w, err)
		return o, false
	}
	if !s.requireOwnerRead(w, p, o.OrgID) {
		return o, false
	}
	if !p.CanManageKeyFor(o.OrgID, o.UserID) {
		forbid(w, "a member can only "+verb+" a key attributed to themselves; "+
			"somebody else's, or one attributed to nobody, is for an "+
			"administrator of this organisation")
		return o, false
	}
	return o, true
}

// keyExpiry reads when a key expires: 'expires_in_seconds' is a lifetime from
// now, 'expires_at' a moment. Neither means the key does not expire.
func keyExpiry(w http.ResponseWriter, in *int64, at *time.Time) (*time.Time, bool) {
	if at != nil {
		if in != nil {
			badRequest(w, "send 'expires_in_seconds' or 'expires_at', not both")
			return nil, false
		}
		if !at.After(time.Now()) {
			badRequest(w, "'expires_at' must be in the future")
			return nil, false
		}
		return at, true
	}
	d, ok := seconds(w, "expires_in_seconds", in)
	if !ok || d == 0 {
		return nil, ok
	}
	t := time.Now().Add(d)
	return &t, true
}

// seconds reads a lifetime given in whole seconds, as every duration on this
// API is. Absent is zero; given, it must be positive.
func seconds(w http.ResponseWriter, field string, v *int64) (time.Duration, bool) {
	switch {
	case v == nil:
		return 0, true
	case *v <= 0 || *v > int64(math.MaxInt64/time.Second):
		badRequest(w, fmt.Sprintf("'%s' must be a positive number of seconds", field))
		return 0, false
	}
	return time.Duration(*v) * time.Second, true
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	keyID := r.PathValue("id")
	o, ok := s.keyToManage(w, r, p, keyID, "revoke")
	if !ok {
		return
	}
	err := s.st.RevokeKey(r.Context(), writeOrg(p), keyID)
	if errors.Is(err, policy.ErrKeyRevoked) {
		conflict(w, "key_revoked", "that key was already revoked")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// The holder tells a member revoking their own leaked key apart from an
	// administrator taking someone's access away.
	s.auditf(r, p, o.OrgID, "key.revoke", "key", keyID, map[string]any{"user_id": o.UserID})
	s.keyChanged(r, keyID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": keyID, "revoked": true})
}
