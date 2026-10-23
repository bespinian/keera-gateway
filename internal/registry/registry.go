// Package registry is the gateway's read-through view of the control plane.
//
// It keeps every organisation's models, filters, routers and MCP servers, and
// recently used keys, in memory, so the inference path reads Postgres only for
// a key it has not seen recently. It refreshes on a timer and drops entries on
// LISTEN/NOTIFY, so a revoked key stops working within a round trip.
//
// In a database outage everything but keys stays as last loaded. A key works
// until its cache entry expires (30 seconds by default); failures are not
// cached, so after that the key is refused as control_plane_unavailable.
package registry

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/secret"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Options tunes the caches.
type Options struct {
	// TTL is how long a resolved key is reused before it is read again.
	TTL time.Duration
	// NegativeTTL is the same for keys that did not resolve. It is short but
	// not zero, so a flood of invalid keys is not a flood of queries.
	NegativeTTL time.Duration
	// SpendRefresh is how often cached spend is reconciled with the database.
	SpendRefresh time.Duration
	// Secrets opens the credentials stored against models and MCP servers.
	// It is required.
	Secrets *secret.Box
	// Platform is the deployment's own provider keys. A model they cover is
	// sent with that key, to the provider's endpoint, at its list prices.
	Platform catalog.Platform
	// Prepaid says organisations pay for those keys in advance, so one
	// without credit cannot use them.
	Prepaid bool
}

// The defaults of the settings behind Options. internal/config reads them
// from here, so the environment and a zero Options mean the same.
const (
	DefaultTTL          = 30 * time.Second
	DefaultSpendRefresh = 10 * time.Second
)

func (o *Options) setDefaults() {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 5 * time.Second
	}
	if o.SpendRefresh <= 0 {
		o.SpendRefresh = DefaultSpendRefresh
	}
}

// refreshEvery is how often everything but spend is reloaded even without a
// notify, in case one was missed.
const refreshEvery = time.Minute

// Source is what the registry reads through. It is an interface so the cache
// behaviour can be tested without Postgres.
type Source interface {
	LookupKey(ctx context.Context, hash []byte) (*policy.Resolved, error)
	LoadModels(ctx context.Context) ([]policy.Model, error)
	LoadFilters(ctx context.Context) ([]policy.Filter, error)
	LoadRouters(ctx context.Context) ([]policy.Router, error)
	LoadMCPServers(ctx context.Context) ([]policy.MCPServer, error)
	ListModels(ctx context.Context, orgID string) ([]policy.Model, error)
	ListFilters(ctx context.Context, orgID string) ([]policy.Filter, error)
	ListRouters(ctx context.Context, orgID string) ([]policy.Router, error)
	ListMCPServers(ctx context.Context, orgID string) ([]policy.MCPServer, error)
	OrgByID(ctx context.Context, orgID string) (store.Org, error)
	LoadSpend(ctx context.Context, now time.Time) ([]store.SpendRow, error)
	LoadCredit(ctx context.Context) ([]store.CreditRow, error)
	LimitedOrgs(ctx context.Context) (map[string]string, error)
	Listen(ctx context.Context, channel string, fn func(payload string)) error
}

// Change is what a policy notify carries: what changed, so each replica drops
// no more than it must. anyChange, and anything this version does not know,
// means anything may have.
type Change string

// anyChange drops every cached key and reloads models, filters, routers and
// MCP servers.
const anyChange Change = ""

const (
	keyChange = "key:"
	orgChange = "org:"
)

// OrgChange is a change inside one organisation, such as a model or a
// guardrail. It drops that organisation's keys and reloads its models,
// filters, routers and MCP servers, and no other's.
func OrgChange(id string) Change { return Change(orgChange + id) }

func (c Change) org() (string, bool) {
	id, ok := strings.CutPrefix(string(c), orgChange)
	return id, ok && id != ""
}

// KeyChange is a change to one key only, such as its creation or revocation.
// It drops that key alone and reloads nothing: a sandbox starting or stopping
// must not cost every active key a query.
func KeyChange(id string) Change { return Change(keyChange + id) }

func (c Change) key() (string, bool) {
	id, ok := strings.CutPrefix(string(c), keyChange)
	return id, ok && id != ""
}

type entry struct {
	resolved *policy.Resolved
	err      error
	expires  time.Time
	gen      uint64
}

// Registry implements policy.Source.
type Registry struct {
	store Source
	opts  Options
	log   *slog.Logger

	// models, filters, routers and MCP servers are keyed by org, then by
	// alias. All four are read on the inference path.
	models  atomic.Pointer[map[string]map[string]policy.Model]
	filters atomic.Pointer[map[string]map[string]policy.Filter]
	routers atomic.Pointer[map[string]map[string]policy.Router]
	mcp     atomic.Pointer[map[string]map[string]policy.MCPServer]

	mu   sync.RWMutex
	keys map[string]entry

	// gen is bumped on every control-plane change. Entries from an older
	// generation are ignored, which drops them all without walking the map.
	gen atomic.Uint64
	// drops is bumped, under mu, each time one key is dropped. A query that
	// started before is not cached, since it may have read the old row.
	drops uint64

	// reload wakes the reload loop. It holds one request, so a burst of
	// changes costs one reload, not one each. pending says what to reload.
	reload  chan struct{}
	pendMu  sync.Mutex
	pending pending

	inflight sync.Map // string -> *call
	// lookups holds a slot for each key query running, up to maxLookups.
	lookups chan struct{}

	budgets *Budgets
}

// New builds a registry and loads everything once, so a gateway that starts
// successfully is a gateway that can serve.
func New(ctx context.Context, st Source, opts Options, log *slog.Logger) (*Registry, error) {
	opts.setDefaults()
	r := &Registry{
		store:   st,
		opts:    opts,
		log:     log,
		keys:    make(map[string]entry),
		budgets: newBudgets(opts.Prepaid),
		lookups: make(chan struct{}, maxLookups),
		reload:  make(chan struct{}, 1),
	}
	if err := r.refreshAll(ctx); err != nil {
		return nil, err
	}
	if err := r.refreshSpend(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Run keeps the caches fresh until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.keepPolicy(ctx) })
	wg.Go(func() { r.keepSpend(ctx) })
	wg.Go(func() { r.listen(ctx) })
	wg.Wait()
}

// keepPolicy reloads what changes ask for, and everything every refreshEvery.
// After New it is the only writer of models, filters, routers and MCP
// servers, so reloading one organisation never races a full reload.
func (r *Registry) keepPolicy(ctx context.Context) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.want(anyChange)
		case <-r.reload:
		}
		if err := r.refreshPending(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("registry refresh failed", "error", err)
		}
	}
}

func (r *Registry) keepSpend(ctx context.Context) {
	t := time.NewTicker(r.opts.SpendRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.refreshSpend(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("registry refresh failed", "error", err)
		}
	}
}

// pending is what the next reload covers: everything, or some organisations.
type pending struct {
	all  bool
	orgs map[string]struct{}
}

// want adds what c makes stale to the next reload. A key change reloads
// nothing.
func (r *Registry) want(c Change) {
	if _, ok := c.key(); ok {
		return
	}
	r.pendMu.Lock()
	defer r.pendMu.Unlock()
	org, ok := c.org()
	switch {
	case r.pending.all:
	case ok:
		if r.pending.orgs == nil {
			r.pending.orgs = make(map[string]struct{})
		}
		r.pending.orgs[org] = struct{}{}
	default:
		r.pending = pending{all: true}
	}
}

// refreshPending runs the reloads asked for so far. What fails is asked for
// again, so the next wake or tick retries it.
func (r *Registry) refreshPending(ctx context.Context) error {
	r.pendMu.Lock()
	p := r.pending
	r.pending = pending{}
	r.pendMu.Unlock()

	if p.all {
		err := r.refreshAll(ctx)
		if err != nil {
			r.want(anyChange)
		}
		return err
	}
	var errs []error
	for org := range p.orgs {
		if err := r.refreshOrg(ctx, org); err != nil {
			r.want(OrgChange(org))
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// listen holds a dedicated connection on the notify channel. Losing it is not
// fatal: the timer-based refresh still bounds staleness, so the loop just
// reconnects.
func (r *Registry) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.listenOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("policy notify listener dropped", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// listenOnce reloads on its own goroutine, not here, so a burst of
// notifications is not a queue of reloads, and a revoked key behind it is
// dropped at once.
func (r *Registry) listenOnce(ctx context.Context) error {
	return r.store.Listen(ctx, store.NotifyChannel, func(payload string) {
		c := Change(payload)
		r.Forget(c)
		if _, ok := c.key(); !ok {
			r.want(c)
			select {
			case r.reload <- struct{}{}:
			default:
			}
		}
	})
}

// Forget drops the cached keys a change makes stale. It is called on notify,
// and directly by the control plane, so its own process does not wait for the
// notification.
func (r *Registry) Forget(c Change) {
	if id, ok := c.key(); ok {
		r.dropKeys(func(k policy.Key) bool { return k.ID == id })
		return
	}
	if id, ok := c.org(); ok {
		r.dropKeys(func(k policy.Key) bool { return k.OrgID == id })
		return
	}
	// Every cached key is dropped by moving on a generation.
	r.gen.Add(1)
}

// dropKeys drops the entries of the keys that match. Unknown keys are cached
// by hash only, so one that turns valid waits out NegativeTTL, which is
// short.
func (r *Registry) dropKeys(match func(policy.Key) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drops++
	for k, e := range r.keys {
		if e.resolved != nil && match(e.resolved.Key) {
			delete(r.keys, k)
		}
	}
}

// refreshAll reloads models, MCP servers, filters and routers in one pass.
// They name each other, so renewing only part could refuse a request for a
// link that exists. A failure part way leaves the rest as they were until the
// next pass.
func (r *Registry) refreshAll(ctx context.Context) error {
	limited, err := r.store.LimitedOrgs(ctx)
	if err != nil {
		return err
	}
	if err := r.refreshModels(ctx, limited); err != nil {
		return err
	}
	if err := r.refreshMCP(ctx, limited); err != nil {
		return err
	}
	if err := reload(ctx, &r.filters, r.store.LoadFilters, func(f *policy.Filter) (string, string) {
		return f.OrgID, f.Alias
	}); err != nil {
		return err
	}
	return reload(ctx, &r.routers, r.store.LoadRouters, func(rt *policy.Router) (string, string) {
		return rt.OrgID, rt.Alias
	})
}

// refreshOrg reloads one organisation's models, MCP servers, filters and
// routers, and leaves every other organisation's as they are. All four are
// read before any is replaced, so a failure changes nothing. An
// organisation that was deleted reads as empty, which removes it.
func (r *Registry) refreshOrg(ctx context.Context, orgID string) error {
	limited := map[string]string{}
	org, err := r.store.OrgByID(ctx, orgID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	case org.Limited:
		limited[orgID] = org.Name
	}
	models, err := r.store.ListModels(ctx, orgID)
	if err != nil {
		return err
	}
	mcp, err := r.store.ListMCPServers(ctx, orgID)
	if err != nil {
		return err
	}
	filters, err := r.store.ListFilters(ctx, orgID)
	if err != nil {
		return err
	}
	routers, err := r.store.ListRouters(ctx, orgID)
	if err != nil {
		return err
	}
	replaceOrg(&r.models, orgID, models, func(m *policy.Model) string {
		r.finishModel(m, limited)
		return m.Alias
	})
	replaceOrg(&r.mcp, orgID, mcp, func(m *policy.MCPServer) string {
		r.finishMCP(m, limited)
		return m.Alias
	})
	replaceOrg(&r.filters, orgID, filters, func(f *policy.Filter) string { return f.Alias })
	replaceOrg(&r.routers, orgID, routers, func(rt *policy.Router) string { return rt.Alias })
	return nil
}

// replaceOrg swaps in a new index with one organisation's things replaced by
// list. The other organisations' maps are shared with the old index, which is
// safe because no index is ever changed in place.
func replaceOrg[T any](into *atomic.Pointer[map[string]map[string]T], orgID string, list []T,
	alias func(*T) string,
) {
	old := *into.Load()
	byOrg := make(map[string]map[string]T, len(old)+1)
	maps.Copy(byOrg, old)
	delete(byOrg, orgID)
	if len(list) > 0 {
		own := make(map[string]T, len(list))
		for i := range list {
			own[alias(&list[i])] = list[i]
		}
		byOrg[orgID] = own
	}
	into.Store(&byOrg)
}

// Filter implements policy.Source.
func (r *Registry) Filter(orgID, alias string) (policy.Filter, bool) {
	return find(&r.filters, orgID, alias)
}

// reload loads one kind of thing and swaps in a new index of it, by org and
// then by alias. key names where each one goes, and may first finish it, such
// as by opening its credential.
//
// The index is replaced whole, never changed in place, so a request reading
// the old one is never half-way through an update.
func reload[T any](ctx context.Context, into *atomic.Pointer[map[string]map[string]T],
	load func(context.Context) ([]T, error), key func(*T) (org, alias string),
) error {
	list, err := load(ctx)
	if err != nil {
		return err
	}
	byOrg := make(map[string]map[string]T)
	for i := range list {
		org, alias := key(&list[i])
		if byOrg[org] == nil {
			byOrg[org] = make(map[string]T)
		}
		byOrg[org][alias] = list[i]
	}
	into.Store(&byOrg)
	return nil
}

func find[T any](index *atomic.Pointer[map[string]map[string]T], orgID, alias string) (T, bool) {
	v, ok := (*index.Load())[orgID][alias]
	return v, ok
}

// Router implements policy.Source.
func (r *Registry) Router(orgID, alias string) (policy.Router, bool) {
	return find(&r.routers, orgID, alias)
}

// Routers implements policy.Source.
func (r *Registry) Routers(orgID string) []policy.Router {
	return sortedValues((*r.routers.Load())[orgID])
}

// sortedValues lists one organisation's things in alias order.
func sortedValues[T any](byAlias map[string]T) []T {
	aliases := slices.Sorted(maps.Keys(byAlias))
	out := make([]T, 0, len(aliases))
	for _, a := range aliases {
		out = append(out, byAlias[a])
	}
	return out
}

// refreshModels reloads the models. limited maps each limited organisation to
// its name.
func (r *Registry) refreshModels(ctx context.Context, limited map[string]string) error {
	return reload(ctx, &r.models, r.store.LoadModels, func(m *policy.Model) (string, string) {
		r.finishModel(m, limited)
		return m.OrgID, m.Alias
	})
}

// finishModel gives a model as stored its credential, and holds it back if
// its organisation is limited.
func (r *Registry) finishModel(m *policy.Model, limited map[string]string) {
	if r.opts.Platform.Covers(*m) {
		r.usePlatformKey(m)
	} else if len(m.APIKeyCiphertext) > 0 {
		m.APIKey = r.open(ModelSecretName(m.OrgID, m.Alias), m.APIKeyCiphertext)
	}
	if name, ok := limited[m.OrgID]; ok {
		m.Limited, m.OrgName = true, name
		m.Locked = !r.openToLimited(*m)
	}
}

// openToLimited reports whether an organisation that signed itself up and has
// not paid yet may use a model: one billed from its credit before it is used,
// or one on a public address, which costs this deployment nothing. A model on
// the deployment's key without payments would be billed after the fact, to
// somebody nobody knows, so it waits too.
func (r *Registry) openToLimited(m policy.Model) bool {
	if m.PlatformKey {
		return r.opts.Prepaid
	}
	return m.Hosting() == policy.HostedExternal
}

// usePlatformKey sends a model with the deployment's own key for its
// provider. A key the organisation stored before is never sent then: the
// endpoint is the provider's, not the one it was entered for.
//
// The endpoint and prices are set again here, not only when the model is
// saved, so a row written before the key was configured cannot send the key
// elsewhere or bill below list price.
func (r *Registry) usePlatformKey(m *policy.Model) {
	if err := r.opts.Platform.Lock(m); err != nil {
		// Left without a key, so the provider refuses it and nothing is
		// sent unbilled.
		r.log.Error("a model cannot use this deployment's provider key",
			"org", m.OrgID, "model", m.Alias, "error", err)
		return
	}
	m.APIKey = r.opts.Platform[m.Provider].APIKey
	m.Billing = &policy.Billing{Provider: m.Provider, Credit: r.opts.Prepaid}
}

func (r *Registry) refreshMCP(ctx context.Context, limited map[string]string) error {
	return reload(ctx, &r.mcp, r.store.LoadMCPServers, func(m *policy.MCPServer) (string, string) {
		r.finishMCP(m, limited)
		return m.OrgID, m.Alias
	})
}

// finishMCP is finishModel for an MCP server.
func (r *Registry) finishMCP(m *policy.MCPServer, limited map[string]string) {
	if len(m.APIKeyCiphertext) > 0 {
		m.APIKey = r.open(MCPSecretName(m.OrgID, m.Alias), m.APIKeyCiphertext)
	}
	if name, ok := limited[m.OrgID]; ok {
		m.Limited, m.OrgName = true, name
		m.Locked = policy.HostingOf(m.URL) != policy.HostedExternal
	}
}

// MCPSecretName is what an MCP server's credential is sealed against. It
// carries the organisation, so it can never be opened as another tenant's,
// and its own prefix, so never as a model's of the same alias.
func MCPSecretName(orgID, alias string) string { return "mcp:" + orgID + ":" + alias }

// MCPServer implements policy.Source.
func (r *Registry) MCPServer(orgID, alias string) (policy.MCPServer, bool) {
	return find(&r.mcp, orgID, alias)
}

// ModelSecretName is what a model's credential is sealed against. As with
// MCPSecretName, the organisation and prefix keep it from opening as another
// tenant's or as an MCP server's.
func ModelSecretName(orgID, alias string) string { return "model:" + orgID + ":" + alias }

// open decrypts one stored credential, once per refresh rather than once per
// request, or logs why it cannot and returns nothing.
//
// A credential that cannot be opened is dropped, not fatal. A gateway whose
// KEERA_SECRET_KEY changed still starts, so the organisation's administrators
// can sign in and set the credential again. It is logged every refresh.
func (r *Registry) open(name string, ciphertext []byte) string {
	plaintext, err := r.opts.Secrets.Open(name, ciphertext)
	if err != nil {
		r.log.Error("a stored credential cannot be decrypted; set it again in the panel or the CLI",
			"credential", name, "error", err)
		return ""
	}
	return plaintext
}

func (r *Registry) refreshSpend(ctx context.Context) error {
	rows, err := r.store.LoadSpend(ctx, time.Now())
	if err != nil {
		return err
	}
	r.budgets.reconcile(rows)
	return r.RefreshCredit(ctx)
}

// RefreshCredit reads every organisation's credit now. The control plane
// calls it when a payment comes in, so the organisation does not wait for
// the next refresh.
func (r *Registry) RefreshCredit(ctx context.Context) error {
	if !r.opts.Prepaid {
		return nil
	}
	rows, err := r.store.LoadCredit(ctx)
	if err != nil {
		return err
	}
	r.budgets.reconcileCredit(rows)
	return nil
}

// Model implements policy.Source.
func (r *Registry) Model(orgID, alias string) (policy.Model, bool) {
	return find(&r.models, orgID, alias)
}

// Models implements policy.Source.
func (r *Registry) Models(orgID string) []policy.Model {
	return sortedValues((*r.models.Load())[orgID])
}

type call struct {
	done     chan struct{}
	resolved *policy.Resolved
	err      error
}

// maxLookups caps the key queries that run at once. The database pool is
// shared with usage writes and the control plane, and a flood of made-up keys
// must not take all of it.
const maxLookups = 4

// errLookupsBusy is what a caller gets who shared a query that never got a
// slot, because the one who started it hung up first.
var errLookupsBusy = errors.New("registry: too many key lookups at once")

// Resolve implements policy.Source.
func (r *Registry) Resolve(ctx context.Context, presented string) (*policy.Resolved, error) {
	// Nothing else can be a key, so it costs no query and no cache entry.
	if !auth.WellFormed(presented) {
		return nil, policy.ErrUnknownKey
	}
	// Keyed by the hash, so a heap dump does not hand out working credentials.
	hash := auth.Hash(presented)
	ck := string(hash)
	gen := r.gen.Load()

	e, drops, ok := r.lookupCache(ck, gen)
	if ok {
		return e.resolved, e.err
	}

	// Concurrent misses on one key share one query, so many editors starting
	// at once do not become many database round trips.
	c := &call{done: make(chan struct{})}
	if actual, loaded := r.inflight.LoadOrStore(ck, c); loaded {
		return wait(ctx, actual.(*call))
	}
	// Waiting for a slot ends when this caller hangs up, so a flood holds
	// no more goroutines than it holds connections.
	select {
	case r.lookups <- struct{}{}:
	case <-ctx.Done():
		c.err = errLookupsBusy
		r.inflight.Delete(ck)
		close(c.done)
		return nil, ctx.Err()
	}
	// The query runs on its own context: others may be waiting on it, and
	// the first caller hanging up must not fail them all.
	go r.lookup(context.WithoutCancel(ctx), hash, ck, gen, drops, c)
	return wait(ctx, c)
}

// lookupTimeout bounds a shared key query, which no caller can cancel.
const lookupTimeout = 10 * time.Second

func (r *Registry) lookup(ctx context.Context, hash []byte, ck string, gen, drops uint64, c *call) {
	defer func() { <-r.lookups }()
	defer r.inflight.Delete(ck)
	defer close(c.done)
	// A query that finished between the check in Resolve and LoadOrStore has
	// already cached its answer, because remember runs before Delete.
	if e, _, ok := r.lookupCache(ck, gen); ok {
		c.resolved, c.err = e.resolved, e.err
		return
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	c.resolved, c.err = r.store.LookupKey(ctx, hash)
	r.remember(ck, gen, drops, c)
}

// lookupCache also returns the drop count it saw, for a query that follows.
func (r *Registry) lookupCache(ck string, gen uint64) (entry, uint64, bool) {
	r.mu.RLock()
	e, ok := r.keys[ck]
	drops := r.drops
	r.mu.RUnlock()
	return e, drops, ok && e.gen == gen && time.Now().Before(e.expires)
}

func wait(ctx context.Context, c *call) (*policy.Resolved, error) {
	select {
	case <-c.done:
		return c.resolved, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// remember caches an answer, never a database failure: an outage must not pin
// a rejection in memory. Nor an answer read before a key was dropped, which
// may be that key as it was.
func (r *Registry) remember(ck string, gen, drops uint64, c *call) {
	if c.err != nil && !isKeyRejection(c.err) {
		return
	}
	ttl := r.opts.TTL
	if c.err != nil {
		ttl = r.opts.NegativeTTL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drops != drops {
		return
	}
	r.keys[ck] = entry{resolved: c.resolved, err: c.err, expires: time.Now().Add(ttl), gen: gen}
}

func isKeyRejection(err error) bool {
	return errors.Is(err, policy.ErrUnknownKey) ||
		errors.Is(err, policy.ErrKeyRevoked) ||
		errors.Is(err, policy.ErrKeyExpired) ||
		errors.Is(err, policy.ErrKeyPending)
}

// Sweep drops expired entries. Without it the key map would grow with every
// token ever presented, including every scan of a public endpoint.
func (r *Registry) Sweep() {
	now := time.Now()
	gen := r.gen.Load()
	r.mu.Lock()
	for k, e := range r.keys {
		if e.gen != gen || now.After(e.expires) {
			delete(r.keys, k)
		}
	}
	r.mu.Unlock()
}

// Budgets exposes the budget view for the gateway's pre-flight check.
func (r *Registry) Budgets() *Budgets { return r.budgets }
