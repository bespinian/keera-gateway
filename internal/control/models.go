package control

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
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
// models exist and their limits.
//
// Backend URLs and whether a key is stored are for the organisation's
// administrators.
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
		// Already left out of JSON by its tag; cleared as well to be safe.
		models[i].APIKeyCiphertext = nil
	}
	s.writeHookList(w, r, models, func(from, to time.Time) (any, error) {
		return s.st.ModelStats(r.Context(), orgID, from, to)
	})
}

// markPlatform shows a model on the deployment's key as the gateway sends
// it: to the provider's endpoint, at its list prices. The row may predate the
// key.
func (s *Server) markPlatform(m *policy.Model) {
	if !s.opts.Platform.Covers(*m) {
		return
	}
	// Its own key is never sent, either way. A model the price table does not
	// know is not on the deployment's key either: the registry sends it
	// without a key, and logs why.
	m.HasAPIKey = false
	_ = s.opts.Platform.Lock(m)
}

// listProviders serves the hosted providers this binary knows, so adding a
// hosted model in the panel needs only a model id. It is the same table the
// template file is read against.
//
// Anyone signed in may read it, for the panel's model catalogue. Only someone who
// can add a model sees where a provider is reached: the form fills it in.
func (s *Server) listProviders(w http.ResponseWriter, _ *http.Request, p *authn.Principal) {
	type entry struct {
		catalog.Provider
		// PlatformKey says the deployment holds the key, so a model of this
		// provider takes none.
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
	// The credential is write-only, so it is part of the request and not of
	// the model. Nil leaves the stored one alone.
	var body struct {
		policy.Model
		APIKey *string `json:"api_key"`
		// Enabled nil means not said: a new model starts enabled, and an
		// existing one keeps what it was.
		Enabled *bool `json:"enabled"`
		// The two prices shadow the model's, so a price left out can be told
		// from one set to 0.
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
	// Locked before the other checks, because it sets the backend they check.
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
	if msg := normalizeModel(&m, priced); msg != "" {
		badRequest(w, msg)
		return
	}
	if m.Subscription && !s.opts.ClaudeSubscriptions {
		badRequest(w, subscriptionsOff)
		return
	}
	if m.Subscription && credential != "" {
		badRequest(w, "a subscription model is sent each caller's own Claude sign-in, so it "+
			"takes no API key")
		return
	}
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
	if err := s.st.UpsertModel(r.Context(), m); err != nil {
		s.fail(w, err)
		return
	}
	// The model marshals without its credential.
	s.auditf(r, p, orgID, "model.put", "model", m.Alias, m)

	// Done here rather than in a client, so the panel, 'keera model add' and
	// 'keera model apply' all leave a new model usable.
	if !found {
		allowed, err := s.st.AllowInOrg(r.Context(), orgID, m.Alias)
		if err != nil {
			s.fail(w, err)
			return
		}
		if allowed {
			s.auditf(r, p, orgID, "guardrail.allow_model", string(policy.ScopeOrg), orgID, m.Alias)
		}
	}

	// A model on the deployment's key, or a subscription model, keeps no key
	// of its own: it would never be used, and a platform model would send it
	// to wherever it points if the deployment's key were taken away.
	if body.APIKey != nil || ((platform || m.Subscription) && found && existing.HasAPIKey) {
		if err := s.setCredential(r, p, orgID, m.Alias, credential, "model",
			registry.ModelSecretName, s.st.SetModelCredential); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.changed(r)
	// Read back, so has_api_key says what is stored and not what this body
	// happened to mention.
	stored, err := s.st.Model(r.Context(), orgID, m.Alias)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.markPlatform(&stored)
	httpx.WriteJSON(w, http.StatusOK, stored)
}

// sameOrigins reports whether every address in next is on a host one in prev
// already was. A stored credential is only ever sent to the hosts it was
// stored for, so whoever moves a model or a server elsewhere has to give it
// again: an administrator could otherwise send a key someone else entered to
// a host of their own.
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

// badAlias refuses an alias that is not one. An alias goes into URLs, command
// lines and client configurations, which quote none of it.
func badAlias(alias string) string {
	return "'" + alias + "' is not a usable alias; use lowercase letters, digits and inner hyphens"
}

// normalizeModel fills in a model's defaults and says what is wrong with it,
// or "" when nothing is. Priced says the caller stated both prices.
func normalizeModel(m *policy.Model, priced bool) string {
	if err := m.CheckBackend(); err != nil {
		return err.Error()
	}
	if m.Kind == "" {
		m.Kind = policy.KindChat
	}
	if !m.Kind.Valid() {
		return "'kind' must be chat, completion, embedding or decision"
	}
	// The provider must be one this build knows, because the gateway reads
	// it to choose how to talk to the backend. Its defaults are not applied
	// here: the caller sends every value, and may have changed any of them.
	// What it can serve and bill is checked as for a template file. putModel
	// has trimmed and lowered it already, since Platform.Covers reads it first.
	provider, known := catalog.ProviderByName(m.Provider)
	if m.Provider != "" && !known {
		return "unknown provider " + strconv.Quote(m.Provider) + "; Keera Gateway knows: " +
			catalog.ProviderNames()
	}
	if known {
		if err := provider.Check(m.Kind, m.BackendModel, priced); err != nil {
			return err.Error()
		}
	}
	// Every model runs somewhere, so an unstated location is the provider's,
	// or read off the backend, as for a template file.
	m.Location = strings.ToLower(strings.TrimSpace(m.Location))
	switch {
	case m.Location == "" && known:
		m.Location = provider.Location
	case m.Location == "":
		loc, err := m.LocationFromBackends()
		if err != nil {
			return err.Error()
		}
		m.Location = loc
	}
	m.ReleaseDate = strings.TrimSpace(m.ReleaseDate)
	m.DisplayName = strings.TrimSpace(m.DisplayName)
	switch {
	case !policy.ValidLocation(m.Location):
		return "'location' must be a short lowercase name, such as ch, usa or onprem"
	case m.ReleaseDate != "" && !policy.ValidReleaseDate(m.ReleaseDate):
		return "'release_date' must be a day written as YYYY-MM-DD"
	}
	if m.LongPrompt != nil {
		if err := m.LongPrompt.Check(); err != nil {
			return err.Error()
		}
	}
	if err := catalog.CheckSubscription(*m); err != nil {
		return err.Error()
	}
	return ""
}

// checkModelAlias refuses a new model named like one of the organisation's
// routers. A model is looked up first, so the router would become
// unreachable.
func (s *Server) checkModelAlias(w http.ResponseWriter, r *http.Request, orgID, alias string) bool {
	_, err := s.st.Router(r.Context(), orgID, alias)
	return !s.aliasTaken(w, err, "'"+alias+"' is already one of this organisation's routers, "+
		"which a model of the same alias would make unreachable - give this model a "+
		"different alias")
}

// checkSubscriptionUsers refuses to make a model a subscription model while a
// filter or router uses it. Only Claude Code signed in to a Claude plan
// reaches such a model, so they would fail on every request. It is the other
// half of the subscription checks in checkReaderModel and
// checkDestinations, since the writes can come in either order.
func (s *Server) checkSubscriptionUsers(w http.ResponseWriter, r *http.Request,
	orgID, alias string) bool {
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
	routers, err := s.st.ListRouters(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	for _, rt := range routers {
		switch {
		case rt.ReadsWith() == alias:
			users = append(users, "router '"+rt.Alias+"' (deciding model)")
		case rt.Offers(alias):
			users = append(users, "router '"+rt.Alias+"' (destination)")
		}
	}
	if len(users) == 0 {
		return true
	}
	conflict(w, "model_in_use",
		"'"+alias+"' is used by "+strings.Join(users, ", ")+". A subscription model is "+
			"reached only by Claude Code signed in to a Claude plan, so they would fail - "+
			"point them at another model first")
	return false
}

// aliasTaken refuses an alias that another kind of thing already has, and
// reports whether it did. err is what looking that thing up returned.
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

// replacing reads the lookup of the row a PUT replaces: found says whether
// there is one. It answers any other error and returns false in ok.
func (s *Server) replacing(w http.ResponseWriter, err error) (found, ok bool) {
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return false, false
	}
	return err == nil, true
}

// enabledAfterPut is what a PUT leaves switched on: what the body says, else
// what was stored, else on for something new.
func enabledAfterPut(said *bool, found, was bool) bool {
	switch {
	case said != nil:
		return *said
	case found:
		return was
	}
	return true
}

// setCredential seals a credential for the thing of kind named by alias, or
// clears it when credential is empty. name is the row's secret name, set the
// store's setter.
func (s *Server) setCredential(r *http.Request, p *authn.Principal,
	orgID, alias, credential, kind string,
	name func(orgID, alias string) string,
	set func(ctx context.Context, orgID, alias string, sealed []byte) error,
) error {
	var sealed []byte
	if credential != "" {
		sealed = s.opts.Secrets.Seal(name(orgID, alias), credential)
	}
	if err := set(r.Context(), orgID, alias, sealed); err != nil {
		return err
	}
	action := kind + ".credential.set"
	if credential == "" {
		action = kind + ".credential.clear"
	}
	// Nothing about the credential goes into the audit log, not even its
	// length. Who set it and when is the record.
	s.auditf(r, p, orgID, action, kind, alias, nil)
	return nil
}

// checkModel tests one model against its own backends. It is for the
// organisation's administrators, because it reveals where the model runs and
// puts load on it.
//
// It offers the model a tool and a question only that tool can answer, so a
// vLLM tool-call parser that does not match the model shows up here.
func (s *Server) checkModel(w http.ResponseWriter, r *http.Request, p *authn.Principal) {
	orgID, ok := s.adminOrg(w, r, p)
	if !ok {
		return
	}
	alias := r.PathValue("alias")
	// The registry, not the store: it holds the decrypted credential the data
	// plane would present.
	m, found := s.reg.Model(orgID, alias)
	if !found {
		s.fail(w, store.ErrNotFound)
		return
	}
	probe := s.opts.Gateway.CheckModel(r.Context(), m)
	// Kept, so an operator can later show when a model last worked.
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
	if err := s.st.DeleteModel(r.Context(), orgID, alias); err != nil {
		s.fail(w, err)
		return
	}
	s.deleted(w, r, p, orgID, "model", alias)
}
