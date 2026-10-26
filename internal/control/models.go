package control

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/httpx"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/registry"
	"github.com/bespinian/keera-gateway/internal/store"
)

// listModels is readable by anyone signed in: a developer needs to know which
// models exist and their limits. Backend URLs and whether a key is stored are
// for the organisation's administrators.
func (s *Server) listModels(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.queryOrg(w, r, p)
	if !ok {
		return
	}
	models, err := s.st.ListModels(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	admin := p.CanAdminOrg(orgID)
	for i := range models {
		s.markPlatform(&models[i])
		if !admin {
			models[i].Backends = nil
			models[i].HasAPIKey = false
		}
		// Its JSON tag already leaves it out; cleared to be safe.
		models[i].APIKeyCiphertext = nil
	}
	s.writeHookList(w, r, models, func(from, to time.Time) (any, error) {
		return s.st.ModelStats(r.Context(), orgID, from, to)
	})
}

// markPlatform shows a model on the deployment's key as the gateway sends it:
// to the provider's endpoint, at list prices. The row may predate the key.
func (s *Server) markPlatform(m *policy.Model) {
	if !s.opts.Platform.Covers(*m) {
		return
	}
	// Its own key is never sent. A model the price table does not know is not
	// on the deployment's key either: the registry sends it without a key, and
	// logs why.
	m.HasAPIKey = false
	_ = s.opts.Platform.Lock(m)
}

// listProviders serves the hosted providers this binary knows, so adding a
// hosted model in the panel needs only a model id.
//
// Anyone signed in may read it. Only someone who can add a model sees the
// endpoints, which the form fills in.
func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request, p *authn.Principal) {
	type entry struct {
		catalog.Provider
		// PlatformKey says the deployment holds the key, so models take none.
		PlatformKey bool `json:"platform_key,omitempty"`
	}
	admin := p.CanAdminOrg(p.OrgID)
	var out []entry
	for _, prov := range catalog.Providers() {
		if !admin {
			prov.Endpoint = ""
		}
		out = append(out, entry{Provider: prov, PlatformKey: s.opts.Platform.Has(prov.Name)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) putModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	// The credential is write-only. Nil leaves the stored one alone.
	var body struct {
		policy.Model
		APIKey *string `json:"api_key"`
		// Nil: a new model starts enabled, an existing one keeps its state.
		Enabled *bool `json:"enabled"`
		// Pointers, so a price left out can be told from one set to 0.
		InputMicrosPerMTok  *int64 `json:"input_micros_per_mtok"`
		OutputMicrosPerMTok *int64 `json:"output_micros_per_mtok"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	m := body.Model
	priced := body.InputMicrosPerMTok != nil && body.OutputMicrosPerMTok != nil
	if body.InputMicrosPerMTok != nil {
		m.InputMicrosPerMTok = *body.InputMicrosPerMTok
	}
	if body.OutputMicrosPerMTok != nil {
		m.OutputMicrosPerMTok = *body.OutputMicrosPerMTok
	}
	m.Alias = r.PathValue("alias")
	m.OrgID = orgID
	if !policy.ValidModelAlias(m.Alias) {
		badRequest(w, "'"+m.Alias+"' is not a usable alias; use lowercase letters, digits, "+
			"inner hyphens and slashes, such as 'anthropic/claude-sonnet-4-5'")
		return
	}
	credential := trimmedCredential(body.APIKey)
	// Locked first, because it sets the backend the other checks read.
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	platform := s.opts.Platform.Covers(m)
	if platform {
		if credential != "" {
			badRequest(w, "this deployment holds the key for "+m.Provider+", so its models "+
				"take no API key")
			return
		}
		if err := s.opts.Platform.Lock(&m); err != nil {
			badRequest(w, err.Error())
			return
		}
		priced = true
	}
	if err := catalog.CheckModel(&m, priced); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !s.subscriptionsAllowed(w, m.Subscription) {
		return
	}
	if m.Subscription && credential != "" {
		badRequest(w, "a subscription model is sent each caller's own Claude sign-in, so it "+
			"takes no API key")
		return
	}
	// Held until the model is saved, so no router takes its alias and no filter
	// or router starts reading with it while it becomes a subscription model.
	unlock, err := s.st.LockReferences(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer unlock()
	existing, err := s.st.Model(r.Context(), orgID, m.Alias)
	found, ok := s.replacing(w, err)
	if !ok || (!found && !s.checkModelAlias(w, r, orgID, m.Alias)) {
		return
	}
	if m.Subscription && !s.checkSubscriptionUsers(w, r, orgID, m.Alias) {
		return
	}
	if !platform && found && existing.HasAPIKey && body.APIKey == nil &&
		!sameOrigins(existing.Backends, m.Backends) {
		badRequest(w, keyNotMoved("backends"))
		return
	}
	m.Enabled = enabledAfterPut(body.Enabled, found, existing.Enabled)
	// A platform or subscription model keeps no key of its own: it would never
	// be used, and a platform model would send it to wherever it points if the
	// deployment's key were taken away.
	setKey := body.APIKey != nil || ((platform || m.Subscription) && found && existing.HasAPIKey)
	var cred store.Credential
	if setKey {
		cred = s.sealCredential(registry.ModelSecretName(orgID, m.Alias), credential)
	}
	if err := s.st.UpsertModel(r.Context(), m, cred); err != nil {
		s.fail(w, err)
		return
	}
	// The model marshals without its credential.
	s.auditf(r, p, orgID, "model.put", "model", m.Alias, m)
	if setKey {
		s.auditCredential(r, p, orgID, "model", m.Alias, credential)
	}

	if !found {
		if err := s.allowInOrg(r, p, orgID, m.Alias); err != nil {
			s.fail(w, err)
			return
		}
	}

	s.changed(r, orgID)
	// Read back, so has_api_key says what is stored.
	stored, err := s.st.Model(r.Context(), orgID, m.Alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.markPlatform(&stored)
	httpx.WriteJSON(w, http.StatusOK, stored)
}

// sameOrigins reports whether every address in next is on a host from prev.
// A stored credential only goes to the hosts it was stored for, so moving a
// model or server elsewhere needs the key again: otherwise an administrator
// could send a key someone else entered to a host of their own.
func sameOrigins(prev, next []string) bool {
	origin := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Scheme + "://" + u.Host)
	}
	known := map[string]bool{}
	for _, b := range prev {
		known[origin(b)] = true
	}
	for _, b := range next {
		if o := origin(b); o == "" || !known[o] {
			return false
		}
	}
	return true
}

// keyNotMoved refuses a change that would send the stored credential to a new
// host.
func keyNotMoved(field string) string {
	return "'" + field + "' now names a host the stored API key was never sent to; " +
		"give 'api_key' again, or \"\" to remove it"
}

// badAlias refuses an invalid alias. An alias goes into URLs, command lines
// and client configurations, which quote none of it.
func badAlias(alias string) string {
	return "'" + alias + "' is not a usable alias; use lowercase letters, digits and inner hyphens"
}

// checkModelAlias refuses a new model named like a router. A model is looked
// up first, so the router would become unreachable.
func (s *Server) checkModelAlias(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	_, err := s.st.Router(r.Context(), orgID, alias)
	return !s.aliasTaken(w, err, "'"+alias+"' is already one of this organisation's routers, "+
		"which a model of the same alias would make unreachable - give this model a "+
		"different alias")
}

// checkSubscriptionUsers refuses to make a model a subscription model while a
// filter or router uses it. Only Claude Code signed in to a Claude plan reaches
// such a model, so they would fail on every request. It is the other half of
// the checks in checkReaderModel and Router.Problem, since the writes can come
// in either order.
func (s *Server) checkSubscriptionUsers(w http.ResponseWriter, r *http.Request,
	orgID, alias string,
) bool {
	return s.checkModelUsers(w, r, orgID, alias,
		"A subscription model is reached only by Claude Code signed in to a Claude plan, "+
			"so they would fail - point them at another model first")
}

// checkModelUsers refuses a change to a model while a filter or router uses
// it, and reports whether it did not. why ends the refusal. The caller holds
// LockReferences.
func (s *Server) checkModelUsers(w http.ResponseWriter, r *http.Request,
	orgID, alias, why string,
) bool {
	filters, err := s.st.ListFilters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	var users []string
	for _, f := range filters {
		if f.ReadsWith() == alias {
			users = append(users, "filter '"+f.Alias+"'")
		}
	}
	routers, err := s.routersUsing(r.Context(), orgID, policy.StepModel, alias)
	if err != nil {
		s.fail(w, err)
		return false
	}
	for _, rtr := range routers {
		users = append(users, "router '"+rtr+"'")
	}
	if len(users) == 0 {
		return true
	}
	conflict(w, "model_in_use", "the model '"+alias+"' is used by "+strings.Join(users, ", ")+". "+why)
	return false
}

// aliasTaken refuses an alias another kind of thing already has, and reports
// whether it did. err is the result of looking that thing up.
func (s *Server) aliasTaken(w http.ResponseWriter, err error, msg string) bool {
	switch {
	case err == nil:
		conflict(w, "alias_in_use", msg)
		return true
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return true
	}
	return false
}

// trimmedCredential reads the write-only credential of a PUT body. Nil leaves
// the stored one alone and "" clears it, so both come out as "".
func trimmedCredential(key *string) string {
	if key == nil {
		return ""
	}
	return strings.TrimSpace(*key)
}

// replacing reads the lookup of the row a PUT replaces. On any error but
// not-found it answers and returns ok false.
func (s *Server) replacing(w http.ResponseWriter, err error) (found, ok bool) {
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return false, false
	}
	return err == nil, true
}

// enabledAfterPut is what the body says, else what was stored, else on.
func enabledAfterPut(said *bool, found, was bool) bool {
	switch {
	case said != nil:
		return *said
	case found:
		return was
	}
	return true
}

// sealCredential seals a credential, or clears the stored one when empty.
func (s *Server) sealCredential(name, credential string) store.Credential {
	if credential == "" {
		return store.ReplaceCredential(nil)
	}
	return store.ReplaceCredential(s.opts.Secrets.Seal(name, credential))
}

// auditCredential records that a credential was set or cleared. Nothing
// about the credential goes into the audit log, not even its length.
func (s *Server) auditCredential(r *http.Request, p *authn.Principal, orgID, kind, alias, credential string) {
	action := kind + ".credential.set"
	if credential == "" {
		action = kind + ".credential.clear"
	}
	s.auditf(r, p, orgID, action, kind, alias, nil)
}

// checkModel tests one model against its own backends. It is for
// administrators: it reveals where the model runs and puts load on it.
//
// It offers the model a tool and a question only that tool can answer, so a
// mismatched vLLM tool-call parser shows up here.
func (s *Server) checkModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	// The registry, not the store: it holds the decrypted credential.
	m, found := s.reg.Model(orgID, alias)
	if !found {
		s.fail(w, store.ErrNotFound)
		return
	}
	probe := s.opts.Gateway.CheckModel(r.Context(), m)
	// Kept, so an operator can see when a model last worked.
	s.auditf(r, p, orgID, "model.check", "model", alias, map[string]any{
		"ok": probe.OK, "status": probe.Status, "tool_calls": probe.ToolCalls,
		"tool_call_as_text": probe.ToolCallAsText, "tools_off": probe.ToolsOff,
		"error": probe.Error,
	})
	httpx.WriteJSON(w, http.StatusOK, probe)
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	unlock, err := s.st.LockReferences(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer unlock()
	if !s.checkModelUsers(w, r, orgID, alias,
		"Every request through them would be refused, so point them at another model first") {
		return
	}
	if err := s.st.DeleteModel(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, "model", alias)
}
