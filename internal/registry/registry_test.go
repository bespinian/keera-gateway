package registry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The registry exists so that nothing on the inference path blocks on Postgres.
// That is a cache in front of authentication, which makes its failure modes the
// interesting ones: a cache that is a little stale serves a revoked key, and a
// cache that remembers the wrong thing can pin a rejection for a key that is
// perfectly good.
//
// The package comment promises two specific things - that a revoked key stops
// working within a round trip rather than within a cache lifetime, and that a
// database outage leaves already-known keys working. Both are below.

// source is a control plane that answers from memory.
type source struct {
	mu sync.Mutex

	keys    map[string]*policy.Resolved
	keyErr  map[string]error
	lookups int32

	models  []policy.Model
	filters []policy.Filter
	routers []policy.Router
	mcp     []policy.MCPServer
	spend   []store.SpendRow

	// loadErr, while set, is what every catalogue read returns.
	loadErr error
	// beforeLookup runs inside LookupKey, which is where a second caller can be
	// let in to race the first.
	beforeLookup func()

	notify func()
}

func newSource() *source {
	return &source{
		keys:   map[string]*policy.Resolved{},
		keyErr: map[string]error{},
	}
}

func (s *source) LookupKey(_ context.Context, hash []byte) (*policy.Resolved, error) {
	atomic.AddInt32(&s.lookups, 1)
	s.mu.Lock()
	before := s.beforeLookup
	s.mu.Unlock()
	if before != nil {
		before()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.keyErr[string(hash)]; ok {
		return nil, err
	}
	if r, ok := s.keys[string(hash)]; ok {
		return r, nil
	}
	return nil, policy.ErrUnknownKey
}

func (s *source) LoadModels(context.Context) ([]policy.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models, s.loadErr
}

func (s *source) LoadFilters(context.Context) ([]policy.Filter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filters, s.loadErr
}

func (s *source) LoadRouters(context.Context) ([]policy.Router, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routers, s.loadErr
}

func (s *source) LoadMCPServers(context.Context) ([]policy.MCPServer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcp, s.loadErr
}

func (s *source) LoadSpend(context.Context, time.Time) ([]store.SpendRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spend, s.loadErr
}

func (s *source) Listen(ctx context.Context, _ string, fn func()) error {
	s.mu.Lock()
	s.notify = fn
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// announce is a control-plane write telling the gateway to drop what it has.
func (s *source) announce() {
	s.mu.Lock()
	fn := s.notify
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (s *source) calls() int { return int(atomic.LoadInt32(&s.lookups)) }

func (s *source) set(key string, r *policy.Resolved) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[hashOf(key)] = r
	delete(s.keyErr, hashOf(key))
}

func (s *source) reject(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, hashOf(key))
	s.keyErr[hashOf(key)] = err
}

// hashOf is how the store is keyed, which is how the cache is keyed too.
func hashOf(key string) string { return string(auth.Hash(key)) }

// cached is how many key entries are being held, which is the number Sweep
// exists to bound.
func (r *Registry) cached() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// expiryOf is when a cached answer runs out, so that the two lifetimes can be
// compared without waiting for either.
func (r *Registry) expiryOf(key string) time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keys[hashOf(key)].expires
}

func resolved(org string) *policy.Resolved {
	return &policy.Resolved{Key: policy.Key{OrgID: org}}
}

func newRegistry(t *testing.T, s *source, opts Options) *Registry {
	t.Helper()
	r, err := New(t.Context(), s, opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestAResolvedKeyIsReadOnceAndThenServedFromMemory(t *testing.T) {
	// The whole point: authentication on the inference path costs a map lookup,
	// not a query. Without this a busy gateway is one database round trip per
	// request before it has done anything at all.
	s := newSource()
	s.set("sk-live", resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Minute})

	for range 10 {
		got, err := r.Resolve(t.Context(), "sk-live")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got.Key.OrgID != "org_1" {
			t.Fatalf("resolved to %q, want org_1", got.Key.OrgID)
		}
	}
	if s.calls() != 1 {
		t.Errorf("made %d lookups for ten requests with the same key, want 1", s.calls())
	}
}

func TestARevokedKeyStopsWorkingWhenTheControlPlaneSaysSo(t *testing.T) {
	// This is the claim in the package comment, and the reason the cache has a
	// generation counter at all. Waiting out a TTL would mean a key revoked
	// because it leaked keeps working for the length of that TTL - which is the
	// one minute during which it matters.
	s := newSource()
	s.set("sk-live", resolved("org_1"))
	// A TTL long enough that expiry cannot be what invalidates it: if this
	// passes, it passed because of the notify.
	r := newRegistry(t, s, Options{TTL: time.Hour})

	go r.Run(t.Context())
	if _, err := r.Resolve(t.Context(), "sk-live"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	s.reject("sk-live", policy.ErrKeyRevoked)
	r.Invalidate()

	if _, err := r.Resolve(t.Context(), "sk-live"); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the key to be refused as revoked", err)
	}
}

func TestANotifyFromTheControlPlaneDropsEveryCachedKey(t *testing.T) {
	// The same thing over the wire rather than in-process: a control plane on
	// another replica revokes a key and announces it over LISTEN/NOTIFY, and
	// every gateway has to let go of what it holds.
	s := newSource()
	s.set("sk-live", resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Hour})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	// Wait for the listener to be registered before announcing to it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		ready := s.notify != nil
		s.mu.Unlock()
		if ready || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := r.Resolve(ctx, "sk-live"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	before := s.calls()

	s.reject("sk-live", policy.ErrKeyRevoked)
	s.announce()

	if _, err := r.Resolve(ctx, "sk-live"); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the key refused after the announcement", err)
	}
	if s.calls() <= before {
		t.Error("the key was answered from the cache after a notify dropped it")
	}
}

func TestADatabaseOutageIsNeverCached(t *testing.T) {
	// The other half of the package comment. A lookup that failed because
	// Postgres was unreachable says nothing about the key, so caching it would
	// let a momentary outage pin a rejection in memory for a whole TTL - long
	// after the database came back.
	s := newSource()
	s.reject("sk-live", errors.New("dial tcp: connection refused"))
	r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Hour})

	if _, err := r.Resolve(t.Context(), "sk-live"); err == nil {
		t.Fatal("Resolve succeeded against an unreachable database")
	}
	// The database comes back, and the very next request has to see that.
	s.set("sk-live", resolved("org_1"))
	got, err := r.Resolve(t.Context(), "sk-live")
	if err != nil {
		t.Fatalf("Resolve after the outage: %v", err)
	}
	if got.Key.OrgID != "org_1" {
		t.Errorf("resolved to %q, want org_1", got.Key.OrgID)
	}
}

func TestARejectedKeyIsCachedButOnlyBriefly(t *testing.T) {
	// A rejection is an answer, so it is cached: otherwise a flood of invalid
	// keys - which is what a public endpoint gets all day - is a flood of
	// queries. It is cached for less time than a good key, because a rejection
	// is the answer most likely to have just changed: somebody being handed a
	// key they are waiting for.
	for _, err := range []error{policy.ErrUnknownKey, policy.ErrKeyRevoked, policy.ErrKeyExpired} {
		t.Run(err.Error(), func(t *testing.T) {
			s := newSource()
			s.reject("sk-bad", err)
			r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Hour})

			for range 5 {
				if _, got := r.Resolve(t.Context(), "sk-bad"); !errors.Is(got, err) {
					t.Fatalf("error = %v, want %v", got, err)
				}
			}
			if s.calls() != 1 {
				t.Errorf("made %d lookups for five presentations of one bad key, want 1",
					s.calls())
			}
		})
	}
}

func TestTheNegativeLifetimeIsShorterThanTheOtherOne(t *testing.T) {
	// Both are cached, and the difference between them is the whole reason
	// there are two settings. A refactor that used one lifetime for both would
	// pass every other test in this file.
	s := newSource()
	s.set("sk-good", resolved("org_1"))
	s.reject("sk-bad", policy.ErrUnknownKey)
	// Long enough that neither expires during the test, so what is being read
	// is the lifetime that was recorded and not the clock.
	r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Minute})

	if _, err := r.Resolve(t.Context(), "sk-good"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), "sk-bad"); err == nil {
		t.Fatal("a bad key resolved")
	}

	good, bad := r.expiryOf("sk-good"), r.expiryOf("sk-bad")
	if good.IsZero() || bad.IsZero() {
		t.Fatal("one of the two was not cached at all")
	}
	if !bad.Before(good) {
		t.Errorf("a rejection is held until %v and an answer until %v; the rejection "+
			"must be the shorter of the two", bad, good)
	}
}

func TestOneMissIsOneQueryHoweverManyAskAtOnce(t *testing.T) {
	// A fleet of editors starting at once - a morning, or a deployment rolling
	// - presents the same key from every process at the same moment. Without
	// the collapse that is one query per request against a database that is
	// already the thing being protected.
	s := newSource()
	s.set("sk-live", resolved("org_1"))

	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(1)
	var once sync.Once
	s.beforeLookup = func() {
		once.Do(func() {
			arrived.Done()
			<-release
		})
	}
	r := newRegistry(t, s, Options{TTL: time.Minute})

	const callers = 20
	var wg sync.WaitGroup
	errs := make([]error, callers)
	orgs := make([]string, callers)
	for i := range callers {
		wg.Go(func() {
			got, err := r.Resolve(t.Context(), "sk-live")
			errs[i] = err
			if got != nil {
				orgs[i] = got.Key.OrgID
			}
		})
		if i == 0 {
			// Let the first one get as far as the lookup, so the other
			// nineteen are certain to find it in flight rather than done.
			arrived.Wait()
		}
	}
	close(release)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if orgs[i] != "org_1" {
			t.Errorf("caller %d resolved to %q, want org_1", i, orgs[i])
		}
	}
	if s.calls() != 1 {
		t.Errorf("made %d lookups for %d concurrent presentations of one key, want 1",
			s.calls(), callers)
	}
}

func TestACallerThatGivesUpDoesNotTakeTheOthersWithIt(t *testing.T) {
	// The collapse makes one caller wait on another's query. A client that
	// hangs up mid-flight must not then be able to fail the request of everyone
	// who joined behind it.
	s := newSource()
	s.set("sk-live", resolved("org_1"))
	release := make(chan struct{})
	var once sync.Once
	var arrived sync.WaitGroup
	arrived.Add(1)
	s.beforeLookup = func() {
		once.Do(func() {
			arrived.Done()
			<-release
		})
	}
	r := newRegistry(t, s, Options{TTL: time.Minute})

	// The one that will do the work.
	first := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), "sk-live")
		first <- err
	}()
	arrived.Wait()

	// The one that joins it and then gives up.
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() {
		_, err := r.Resolve(ctx, "sk-live")
		joined <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := <-joined; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller that gave up got %v, want a cancellation", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the caller doing the work was failed by one that left: %v", err)
	}
}

func TestSweepForgetsWhatIsExpiredAndWhatIsStale(t *testing.T) {
	// Without it the key map grows with every distinct token ever presented,
	// which on a public endpoint is every scan. Two things make an entry
	// collectable: it has run out, or the generation it was minted in is over.
	s := newSource()
	s.set("sk-a", resolved("org_1"))
	s.reject("sk-b", policy.ErrUnknownKey)
	r := newRegistry(t, s, Options{TTL: time.Nanosecond, NegativeTTL: time.Nanosecond})

	if _, err := r.Resolve(t.Context(), "sk-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), "sk-b"); err == nil {
		t.Fatal("a bad key resolved")
	}
	if n := r.cached(); n != 2 {
		t.Fatalf("cached %d entries, want 2", n)
	}

	time.Sleep(time.Millisecond)
	r.Sweep()
	if n := r.cached(); n != 0 {
		t.Errorf("cached %d entries after sweeping expired ones, want 0", n)
	}

	// And a generation that has moved on, without waiting for any expiry.
	r2 := newRegistry(t, s, Options{TTL: time.Hour})
	if _, err := r2.Resolve(t.Context(), "sk-a"); err != nil {
		t.Fatal(err)
	}
	if n := r2.cached(); n != 1 {
		t.Fatalf("cached %d entries, want 1", n)
	}
	r2.Invalidate()
	r2.Sweep()
	if n := r2.cached(); n != 0 {
		t.Errorf("cached %d entries after invalidating and sweeping, want 0", n)
	}
}

func TestTheCacheIsKeyedByHashAndNotByTheKeyItself(t *testing.T) {
	// A heap dump of a running gateway is taken by whoever is debugging it and
	// ends up wherever such things end up. It must not contain working
	// credentials, so the cache holds what the database holds.
	s := newSource()
	s.set("sk-live-secret-value", resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Minute})
	if _, err := r.Resolve(t.Context(), "sk-live-secret-value"); err != nil {
		t.Fatal(err)
	}

	for k := range r.keys {
		if k == "sk-live-secret-value" {
			t.Fatal("the presented key is a cache key; a heap dump would hand it out")
		}
		if k != hashOf("sk-live-secret-value") {
			t.Errorf("cache key %q is neither the key nor its hash", k)
		}
	}
}

func TestAGatewayDoesNotStartWithoutACatalogue(t *testing.T) {
	// New loads once before returning, so that a gateway which has started is
	// a gateway that can serve. Starting anyway would mean the first requests
	// after a deployment are refused for a model that exists.
	s := newSource()
	s.loadErr = errors.New("connection refused")
	if _, err := New(t.Context(), s, Options{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("New succeeded with a database it could not read")
	}
}

func TestTheCatalogueIsReplacedWholeOrNotAtAll(t *testing.T) {
	// A guardrail that names a filter, a router that names a destination and a
	// filter that names a model are one chain. A refresh that renewed part of
	// it would leave requests refused for a link that already exists.
	s := newSource()
	s.models = []policy.Model{{Alias: "small"}}
	s.filters = []policy.Filter{{OrgID: "org_1", Alias: "pii"}}
	s.routers = []policy.Router{{OrgID: "org_1", Alias: "cheap-first"}}
	r := newRegistry(t, s, Options{})

	if _, ok := r.Model("small"); !ok {
		t.Fatal("the catalogue did not load")
	}

	// The models now read, the filters do not. Nothing may be adopted.
	s.mu.Lock()
	s.models = []policy.Model{{Alias: "small"}, {Alias: "large"}}
	s.loadErr = errors.New("connection refused")
	s.mu.Unlock()

	if err := r.refreshCatalogue(t.Context()); err == nil {
		t.Fatal("refreshCatalogue succeeded with a database it could not read")
	}
	if _, ok := r.Model("large"); ok {
		t.Error("half of a failed refresh was adopted")
	}
	if _, ok := r.Filter("org_1", "pii"); !ok {
		t.Error("a failed refresh threw away the filters that were already loaded")
	}
}

func TestDefaultsAreFilledInForWhateverIsNotSet(t *testing.T) {
	var o Options
	o.setDefaults()
	if o.TTL != 30*time.Second {
		t.Errorf("TTL = %v, want 30s", o.TTL)
	}
	// Shorter than TTL, and not zero: zero would make every invalid key a query.
	if o.NegativeTTL != 5*time.Second {
		t.Errorf("NegativeTTL = %v, want 5s", o.NegativeTTL)
	}
	if o.NegativeTTL >= o.TTL {
		t.Errorf("NegativeTTL %v is not shorter than TTL %v", o.NegativeTTL, o.TTL)
	}
	if o.ModelRefresh != time.Minute {
		t.Errorf("ModelRefresh = %v, want 1m", o.ModelRefresh)
	}
	if o.SpendRefresh != 10*time.Second {
		t.Errorf("SpendRefresh = %v, want 10s", o.SpendRefresh)
	}
}

// Keeps the compiler honest about the store still being a Source, which is the
// only thing that makes any of the above worth running.
var _ Source = (*store.Store)(nil)
