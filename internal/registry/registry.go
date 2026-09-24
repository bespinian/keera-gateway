// Package registry is the gateway's read-through view of the control plane.
//
// Nothing on the inference path may block on Postgres. The registry keeps the
// catalogue and recently used keys in memory, refreshes them on a timer, and
// drops them on LISTEN/NOTIFY. So a revoked key stops working within a round
// trip, and a database outage leaves already-known keys working.
package registry

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
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
	// ModelRefresh is how often the catalogue is reloaded even without a notify.
	ModelRefresh time.Duration
	// SpendRefresh is how often cached spend is reconciled with the database.
	SpendRefresh time.Duration
	// Secrets opens credentials stored against a model. Nil, when no
	// encryption key is configured, leaves those models on api_key_env.
	Secrets *secret.Box
}

func (o *Options) setDefaults() {
	if o.TTL <= 0 {
		o.TTL = 30 * time.Second
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 5 * time.Second
	}
	if o.ModelRefresh <= 0 {
		o.ModelRefresh = time.Minute
	}
	if o.SpendRefresh <= 0 {
		o.SpendRefresh = 10 * time.Second
	}
}

// Source is what the registry reads through. It is an interface so the cache
// behaviour can be tested without Postgres.
type Source interface {
	LookupKey(ctx context.Context, hash []byte) (*policy.Resolved, error)
	LoadModels(ctx context.Context) ([]policy.Model, error)
	LoadFilters(ctx context.Context) ([]policy.Filter, error)
	LoadRouters(ctx context.Context) ([]policy.Router, error)
	LoadMCPServers(ctx context.Context) ([]policy.MCPServer, error)
	LoadSpend(ctx context.Context, now time.Time) ([]store.SpendRow, error)
	Listen(ctx context.Context, channel string, fn func()) error
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

	models atomic.Pointer[map[string]policy.Model]
	mcp    atomic.Pointer[map[string]policy.MCPServer]
	// filters and routers are keyed by org, then by alias. Both are read on
	// the inference path.
	filters atomic.Pointer[map[string]map[string]policy.Filter]
	routers atomic.Pointer[map[string]map[string]policy.Router]
	secrets *secret.Box

	mu   sync.RWMutex
	keys map[string]entry

	// gen is bumped on every control-plane change. Entries from an older
	// generation are ignored, which drops them all without walking the map.
	gen atomic.Uint64

	inflight sync.Map // string -> *call

	budgets *Budgets
}

// New builds a registry and loads the catalogue once, so a gateway that starts
// successfully is a gateway that can serve.
func New(ctx context.Context, st Source, opts Options, log *slog.Logger) (*Registry, error) {
	opts.setDefaults()
	r := &Registry{
		store:   st,
		opts:    opts,
		log:     log,
		secrets: opts.Secrets,
		keys:    make(map[string]entry),
		budgets: newBudgets(),
	}
	if err := r.refreshCatalogue(ctx); err != nil {
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
	wg.Add(3)
	go func() { defer wg.Done(); r.loop(ctx, r.opts.ModelRefresh, r.refreshCatalogue) }()
	go func() { defer wg.Done(); r.loop(ctx, r.opts.SpendRefresh, r.refreshSpend) }()
	go func() { defer wg.Done(); r.listen(ctx) }()
	wg.Wait()
}

func (r *Registry) loop(ctx context.Context, every time.Duration, fn func(context.Context) error) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("registry refresh failed", "error", err)
			}
		}
	}
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

func (r *Registry) listenOnce(ctx context.Context) error {
	return r.store.Listen(ctx, store.NotifyChannel, func() {
		r.Invalidate()
		if err := r.refreshCatalogue(ctx); err != nil {
			r.log.Warn("catalogue refresh after notify failed", "error", err)
		}
	})
}

// Invalidate drops every cached key. Called on notify, and directly by the
// control plane when both listeners live in the same process.
func (r *Registry) Invalidate() { r.gen.Add(1) }

// refreshCatalogue reloads models, filters and routers together. They name each
// other, so renewing only part of the chain could refuse a request for a link
// that already exists. MCP servers come along, being catalogue too.
func (r *Registry) refreshCatalogue(ctx context.Context) error {
	if err := r.refreshModels(ctx); err != nil {
		return err
	}
	if err := r.refreshMCP(ctx); err != nil {
		return err
	}
	if err := r.refreshFilters(ctx); err != nil {
		return err
	}
	return r.refreshRouters(ctx)
}

func (r *Registry) refreshFilters(ctx context.Context) error {
	list, err := r.store.LoadFilters(ctx)
	if err != nil {
		return err
	}
	byOrg := groupByOrg(list, func(f policy.Filter) (string, string) { return f.OrgID, f.Alias })
	r.filters.Store(&byOrg)
	return nil
}

// Filter implements policy.Source.
func (r *Registry) Filter(orgID, alias string) (policy.Filter, bool) {
	f, ok := (*r.filters.Load())[orgID][alias]
	return f, ok
}

func (r *Registry) refreshRouters(ctx context.Context) error {
	list, err := r.store.LoadRouters(ctx)
	if err != nil {
		return err
	}
	byOrg := groupByOrg(list, func(rt policy.Router) (string, string) { return rt.OrgID, rt.Alias })
	r.routers.Store(&byOrg)
	return nil
}

// groupByOrg indexes list by org and then by alias.
func groupByOrg[T any](list []T, key func(T) (org, alias string)) map[string]map[string]T {
	byOrg := make(map[string]map[string]T)
	for _, v := range list {
		org, alias := key(v)
		if byOrg[org] == nil {
			byOrg[org] = make(map[string]T)
		}
		byOrg[org][alias] = v
	}
	return byOrg
}

// Router implements policy.Source.
func (r *Registry) Router(orgID, alias string) (policy.Router, bool) {
	rt, ok := (*r.routers.Load())[orgID][alias]
	return rt, ok
}

// Routers implements policy.Source.
func (r *Registry) Routers(orgID string) []policy.Router {
	byAlias := (*r.routers.Load())[orgID]
	out := make([]policy.Router, 0, len(byAlias))
	for _, rt := range byAlias {
		out = append(out, rt)
	}
	slices.SortFunc(out, func(a, b policy.Router) int { return cmp.Compare(a.Alias, b.Alias) })
	return out
}

func (r *Registry) refreshModels(ctx context.Context) error {
	list, err := r.store.LoadModels(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]policy.Model, len(list))
	for _, mod := range list {
		m[mod.Alias] = r.decrypt(mod)
	}
	r.models.Store(&m)
	return nil
}

func (r *Registry) refreshMCP(ctx context.Context) error {
	list, err := r.store.LoadMCPServers(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]policy.MCPServer, len(list))
	for _, srv := range list {
		if len(srv.APIKeyCiphertext) > 0 {
			srv.APIKey = r.open(MCPSecretName(srv.Alias), srv.APIKeyCiphertext)
		}
		m[srv.Alias] = srv
	}
	r.mcp.Store(&m)
	return nil
}

// MCPSecretName is what an MCP server's credential is sealed against. It is
// not the bare alias, so a server's sealed credential can never be opened as
// the credential of a model with the same alias.
func MCPSecretName(alias string) string { return "mcp:" + alias }

// MCPServer implements policy.Source.
func (r *Registry) MCPServer(alias string) (policy.MCPServer, bool) {
	m, ok := (*r.mcp.Load())[alias]
	return m, ok
}

// decrypt opens a model's stored credential, once per refresh rather than once
// per request.
//
// A credential that cannot be opened is dropped, not fatal. The model can still
// use api_key_env, and a gateway whose KEERA_SECRET_KEY changed still starts,
// so an operator can sign in and set the key again. It is logged every refresh.
func (r *Registry) decrypt(m policy.Model) policy.Model {
	if len(m.APIKeyCiphertext) == 0 {
		return m
	}
	m.APIKey = r.open(m.Alias, m.APIKeyCiphertext)
	return m
}

// open decrypts one stored credential, or logs why it cannot and returns
// nothing.
func (r *Registry) open(name string, ciphertext []byte) string {
	if !r.secrets.Enabled() {
		r.log.Error("a stored credential cannot be read because no encryption key is "+
			"configured; set KEERA_SECRET_KEY", "alias", name)
		return ""
	}
	plaintext, err := r.secrets.Open(name, ciphertext)
	if err != nil {
		r.log.Error("a stored credential cannot be decrypted; set it again in the panel",
			"alias", name, "error", err)
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
	return nil
}

// Model implements policy.Source.
func (r *Registry) Model(alias string) (policy.Model, bool) {
	m, ok := (*r.models.Load())[alias]
	return m, ok
}

// Models implements policy.Source.
func (r *Registry) Models() []policy.Model {
	m := *r.models.Load()
	out := make([]policy.Model, 0, len(m))
	for _, mod := range m {
		out = append(out, mod)
	}
	return out
}

type call struct {
	done     chan struct{}
	resolved *policy.Resolved
	err      error
}

// Resolve implements policy.Source.
func (r *Registry) Resolve(ctx context.Context, presented string) (*policy.Resolved, error) {
	// Keyed by the hash, so a heap dump does not hand out working credentials.
	hash := auth.Hash(presented)
	ck := string(hash)
	gen := r.gen.Load()

	if e, ok := r.lookupCache(ck, gen); ok {
		return e.resolved, e.err
	}

	// Concurrent misses on one key share one query, so many editors starting
	// at once do not become many database round trips.
	c := &call{done: make(chan struct{})}
	if actual, loaded := r.inflight.LoadOrStore(ck, c); loaded {
		return wait(ctx, actual.(*call))
	}
	// A query that finished between the check above and LoadOrStore has
	// already cached its answer, because remember runs before Delete.
	if e, ok := r.lookupCache(ck, gen); ok {
		c.resolved, c.err = e.resolved, e.err
	} else {
		c.resolved, c.err = r.store.LookupKey(ctx, hash)
		r.remember(ck, gen, c)
	}
	close(c.done)
	r.inflight.Delete(ck)
	return c.resolved, c.err
}

func (r *Registry) lookupCache(ck string, gen uint64) (entry, bool) {
	r.mu.RLock()
	e, ok := r.keys[ck]
	r.mu.RUnlock()
	return e, ok && e.gen == gen && time.Now().Before(e.expires)
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
// a rejection in memory.
func (r *Registry) remember(ck string, gen uint64, c *call) {
	if c.err != nil && !isKeyRejection(c.err) {
		return
	}
	ttl := r.opts.TTL
	if c.err != nil {
		ttl = r.opts.NegativeTTL
	}
	r.mu.Lock()
	r.keys[ck] = entry{resolved: c.resolved, err: c.err, expires: time.Now().Add(ttl), gen: gen}
	r.mu.Unlock()
}

func isKeyRejection(err error) bool {
	return errors.Is(err, policy.ErrUnknownKey) ||
		errors.Is(err, policy.ErrKeyRevoked) ||
		errors.Is(err, policy.ErrKeyExpired)
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
