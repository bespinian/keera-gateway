package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-gateway/internal/auth"
	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// The registry is a cache in front of authentication, so its failure modes
// matter most: a stale cache serves a revoked key, and one that remembers the
// wrong thing pins a rejection for a good key.

// source is a control plane that answers from memory.
type source struct {
	mu sync.Mutex

	keys    map[string]*policy.Resolved
	keyErr  map[string]error
	lookups atomic.Int32

	models  []policy.Model
	filters []policy.Filter
	routers []policy.Router
	mcp     []policy.MCPServer
	spend   []store.SpendRow
	credit  []store.CreditRow
	limited map[string]string

	// fullLoads and orgLoads count model reads of every organisation and of
	// one.
	fullLoads, orgLoads int

	// loadErr, while set, is what every catalogue read returns.
	loadErr error
	// beforeLookup runs inside LookupKey, which is where a second caller can be
	// let in to race the first.
	beforeLookup func()

	notify func(string)
}

func newSource() *source {
	return &source{
		keys:   map[string]*policy.Resolved{},
		keyErr: map[string]error{},
	}
}

func (s *source) LookupKey(ctx context.Context, hash []byte) (*policy.Resolved, error) {
	s.lookups.Add(1)
	s.mu.Lock()
	before := s.beforeLookup
	s.mu.Unlock()
	if before != nil {
		before()
	}
	// As Postgres does: a query whose context is gone fails.
	if err := ctx.Err(); err != nil {
		return nil, err
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
	s.fullLoads++
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

// inOrg keeps the things of one organisation, as the List queries do.
func inOrg[T any](list []T, orgID string, org func(T) string) []T {
	var out []T
	for _, v := range list {
		if org(v) == orgID {
			out = append(out, v)
		}
	}
	return out
}

func (s *source) ListModels(_ context.Context, orgID string) ([]policy.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgLoads++
	return inOrg(s.models, orgID, func(m policy.Model) string { return m.OrgID }), s.loadErr
}

func (s *source) ListFilters(_ context.Context, orgID string) ([]policy.Filter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return inOrg(s.filters, orgID, func(f policy.Filter) string { return f.OrgID }), s.loadErr
}

func (s *source) ListRouters(_ context.Context, orgID string) ([]policy.Router, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return inOrg(s.routers, orgID, func(rt policy.Router) string { return rt.OrgID }), s.loadErr
}

func (s *source) ListMCPServers(_ context.Context, orgID string) ([]policy.MCPServer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return inOrg(s.mcp, orgID, func(m policy.MCPServer) string { return m.OrgID }), s.loadErr
}

func (s *source) OrgByID(_ context.Context, orgID string) (store.Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return store.Org{}, s.loadErr
	}
	name, limited := s.limited[orgID]
	return store.Org{ID: orgID, Name: name, Limited: limited}, nil
}

func (s *source) LoadSpend(context.Context, time.Time) ([]store.SpendRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spend, s.loadErr
}

func (s *source) LoadCredit(context.Context) ([]store.CreditRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credit, s.loadErr
}

func (s *source) LimitedOrgs(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limited, s.loadErr
}

func (s *source) Listen(ctx context.Context, _ string, fn func(string)) error {
	s.mu.Lock()
	s.notify = fn
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// announce is a control-plane write telling the gateway to drop what it has.
func (s *source) announce(c Change) {
	s.mu.Lock()
	fn := s.notify
	s.mu.Unlock()
	if fn != nil {
		fn(string(c))
	}
}

// waitForListener waits until Run listens, so an announcement reaches it.
func waitForListener(t *testing.T, s *source) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		ready := s.notify != nil
		s.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the registry never started listening")
		}
		time.Sleep(time.Millisecond)
	}
}

func (s *source) calls() int { return int(s.lookups.Load()) }

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

// testKey turns a name into a well-formed key.
func testKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return auth.Prefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

// hashOf is how the store is keyed, which is how the cache is keyed too.
func hashOf(key string) string { return string(auth.Hash(key)) }

// cached is how many key entries are held, the number Sweep bounds.
func (r *Registry) cached() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.keys)
}

// expiryOf lets the two lifetimes be compared without waiting for either.
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
	// Authentication on the inference path costs a map lookup, not a query.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Minute})

	for range 10 {
		got, err := r.Resolve(t.Context(), testKey("sk-live"))
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
	// This is why the cache has a generation counter. Waiting out a TTL would
	// let a leaked key work for exactly the minute that matters.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))
	// A TTL long enough that expiry cannot be what invalidates it: if this
	// passes, it passed because of the notify.
	r := newRegistry(t, s, Options{TTL: time.Hour})

	go r.Run(t.Context())
	if _, err := r.Resolve(t.Context(), testKey("sk-live")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	s.reject(testKey("sk-live"), policy.ErrKeyRevoked)
	r.Forget(anyChange)

	if _, err := r.Resolve(t.Context(), testKey("sk-live")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the key to be refused as revoked", err)
	}
}

func TestANotifyFromTheControlPlaneDropsEveryCachedKey(t *testing.T) {
	// The same over the wire: a control plane on another replica revokes a key
	// and announces it over LISTEN/NOTIFY.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Hour})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)
	waitForListener(t, s)

	if _, err := r.Resolve(ctx, testKey("sk-live")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	before := s.calls()

	s.reject(testKey("sk-live"), policy.ErrKeyRevoked)
	s.announce(anyChange)

	if _, err := r.Resolve(ctx, testKey("sk-live")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the key refused after the announcement", err)
	}
	if s.calls() <= before {
		t.Error("the key was answered from the cache after a notify dropped it")
	}
}

func TestAKeyChangeDropsThatKeyAlone(t *testing.T) {
	// A sandbox mints and revokes a key each time it starts and stops. If that
	// dropped every key, each one in use would cost a query on every replica.
	s := newSource()
	revoked := &policy.Resolved{Key: policy.Key{ID: "key_1", OrgID: "org_1"}}
	kept := &policy.Resolved{Key: policy.Key{ID: "key_2", OrgID: "org_1"}}
	s.set(testKey("sk-one"), revoked)
	s.set(testKey("sk-two"), kept)
	r := newRegistry(t, s, Options{TTL: time.Hour})

	for _, k := range []string{"sk-one", "sk-two"} {
		if _, err := r.Resolve(t.Context(), testKey(k)); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	before := s.calls()

	s.reject(testKey("sk-one"), policy.ErrKeyRevoked)
	r.Forget(KeyChange("key_1"))

	if _, err := r.Resolve(t.Context(), testKey("sk-one")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the dropped key refused as revoked", err)
	}
	if _, err := r.Resolve(t.Context(), testKey("sk-two")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := s.calls() - before; got != 1 {
		t.Errorf("%d lookups after one key changed, want 1: the other key was dropped too", got)
	}
}

func TestAnOrgChangeDropsThatOrganisationsKeysAlone(t *testing.T) {
	// A guardrail changed in one organisation must not cost every other
	// organisation's keys a query.
	s := newSource()
	s.set(testKey("sk-mine"), &policy.Resolved{Key: policy.Key{ID: "key_1", OrgID: "org_1"}})
	s.set(testKey("sk-theirs"), &policy.Resolved{Key: policy.Key{ID: "key_2", OrgID: "org_2"}})
	r := newRegistry(t, s, Options{TTL: time.Hour})

	for _, k := range []string{"sk-mine", "sk-theirs"} {
		if _, err := r.Resolve(t.Context(), testKey(k)); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	before := s.calls()

	s.reject(testKey("sk-mine"), policy.ErrKeyRevoked)
	r.Forget(OrgChange("org_1"))

	if _, err := r.Resolve(t.Context(), testKey("sk-mine")); !errors.Is(err, policy.ErrKeyRevoked) {
		t.Errorf("error = %v, want the organisation's key read again", err)
	}
	if _, err := r.Resolve(t.Context(), testKey("sk-theirs")); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := s.calls() - before; got != 1 {
		t.Errorf("%d lookups after one organisation changed, want 1", got)
	}
}

func TestAnOrgChangeReloadsThatOrganisationAlone(t *testing.T) {
	s := newSource()
	s.models = []policy.Model{
		{OrgID: "org_1", Alias: "fast", BackendModel: "old"},
		{OrgID: "org_2", Alias: "fast", BackendModel: "old"},
	}
	s.filters = []policy.Filter{{OrgID: "org_2", Alias: "pii"}}
	r := newRegistry(t, s, Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)
	waitForListener(t, s)

	s.mu.Lock()
	s.models = []policy.Model{
		{OrgID: "org_1", Alias: "fast", BackendModel: "new"},
		{OrgID: "org_1", Alias: "engine", Backends: []string{"http://keera-engine:8000/v1"}},
		{OrgID: "org_2", Alias: "fast", BackendModel: "new"},
	}
	s.limited = map[string]string{"org_1": "Example Bank"}
	full := s.fullLoads
	s.mu.Unlock()
	s.announce(OrgChange("org_1"))

	deadline := time.Now().Add(2 * time.Second)
	for {
		if m, _ := r.Model("org_1", "fast"); m.BackendModel == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the organisation that changed was not reloaded")
		}
		time.Sleep(time.Millisecond)
	}
	if m, _ := r.Model("org_1", "engine"); !m.Limited || !m.Locked {
		t.Errorf("engine: limited %v, locked %v; want the organisation's limit applied", m.Limited, m.Locked)
	}
	if m, _ := r.Model("org_2", "fast"); m.BackendModel != "old" {
		t.Error("another organisation was reloaded too")
	}
	if _, ok := r.Filter("org_2", "pii"); !ok {
		t.Error("another organisation lost its filter")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fullLoads != full {
		t.Errorf("%d full reloads after one organisation changed, want none", s.fullLoads-full)
	}
}

func TestADeletedOrganisationIsDropped(t *testing.T) {
	s := newSource()
	s.models = []policy.Model{{OrgID: "org_1", Alias: "fast"}, {OrgID: "org_2", Alias: "fast"}}
	r := newRegistry(t, s, Options{})

	s.mu.Lock()
	s.models = []policy.Model{{OrgID: "org_2", Alias: "fast"}}
	s.mu.Unlock()
	if err := r.refreshOrg(t.Context(), "org_1"); err != nil {
		t.Fatalf("refreshOrg: %v", err)
	}
	if _, ok := r.Model("org_1", "fast"); ok {
		t.Error("a deleted organisation's model is still served")
	}
	if _, ok := r.Model("org_2", "fast"); !ok {
		t.Error("another organisation's model went with it")
	}
}

func TestAFailedOrgReloadIsTriedAgain(t *testing.T) {
	// Only the next tick would reload the organisation otherwise, and a
	// change that is not retried waits for it.
	s := newSource()
	s.models = []policy.Model{{OrgID: "org_1", Alias: "fast", BackendModel: "old"}}
	r := newRegistry(t, s, Options{})

	s.mu.Lock()
	s.models = []policy.Model{{OrgID: "org_1", Alias: "fast", BackendModel: "new"}}
	s.loadErr = errors.New("connection refused")
	s.mu.Unlock()
	r.want(OrgChange("org_1"))
	if err := r.refreshPending(t.Context()); err == nil {
		t.Fatal("refreshPending succeeded with a database it could not read")
	}
	if m, _ := r.Model("org_1", "fast"); m.BackendModel != "old" {
		t.Error("part of a failed reload was adopted")
	}

	s.mu.Lock()
	s.loadErr = nil
	s.mu.Unlock()
	if err := r.refreshPending(t.Context()); err != nil {
		t.Fatalf("refreshPending: %v", err)
	}
	if m, _ := r.Model("org_1", "fast"); m.BackendModel != "new" {
		t.Error("the failed reload was not tried again")
	}
}

func TestAChangeOfUnknownShapeReloadsEverything(t *testing.T) {
	// A replica of a newer version may announce a change this one does not
	// know, and an older one sends anyChange for all of them.
	r := newRegistry(t, newSource(), Options{})
	for _, c := range []Change{anyChange, "org:", "project:prj_1"} {
		r.want(OrgChange("org_1"))
		r.want(c)
		if !r.pending.all {
			t.Errorf("%q does not reload everything", c)
		}
		r.pending = pending{}
	}
}

func TestADatabaseOutageIsNeverCached(t *testing.T) {
	// A lookup that failed because Postgres was unreachable says nothing about
	// the key. Caching it would pin a rejection for a whole TTL after the
	// database came back.
	s := newSource()
	s.reject(testKey("sk-live"), errors.New("dial tcp: connection refused"))
	r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Hour})

	if _, err := r.Resolve(t.Context(), testKey("sk-live")); err == nil {
		t.Fatal("Resolve succeeded against an unreachable database")
	}
	// The database comes back, and the very next request has to see that.
	s.set(testKey("sk-live"), resolved("org_1"))
	got, err := r.Resolve(t.Context(), testKey("sk-live"))
	if err != nil {
		t.Fatalf("Resolve after the outage: %v", err)
	}
	if got.Key.OrgID != "org_1" {
		t.Errorf("resolved to %q, want org_1", got.Key.OrgID)
	}
}

func TestARejectedKeyIsCachedButOnlyBriefly(t *testing.T) {
	// A rejection is cached, or a flood of invalid keys is a flood of queries.
	// It is cached for less time than a good key, because it is the answer most
	// likely to change soon: somebody waiting for a key they were just given.
	for _, err := range []error{policy.ErrUnknownKey, policy.ErrKeyRevoked, policy.ErrKeyExpired} {
		t.Run(err.Error(), func(t *testing.T) {
			s := newSource()
			s.reject(testKey("sk-bad"), err)
			r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Hour})

			for range 5 {
				if _, got := r.Resolve(t.Context(), testKey("sk-bad")); !errors.Is(got, err) {
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
	// A refactor that used one lifetime for both would pass every other test
	// in this file.
	s := newSource()
	s.set(testKey("sk-good"), resolved("org_1"))
	s.reject(testKey("sk-bad"), policy.ErrUnknownKey)
	// Long enough that neither expires during the test.
	r := newRegistry(t, s, Options{TTL: time.Hour, NegativeTTL: time.Minute})

	if _, err := r.Resolve(t.Context(), testKey("sk-good")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), testKey("sk-bad")); err == nil {
		t.Fatal("a bad key resolved")
	}

	good, bad := r.expiryOf(testKey("sk-good")), r.expiryOf(testKey("sk-bad"))
	if good.IsZero() || bad.IsZero() {
		t.Fatal("one of the two was not cached at all")
	}
	if !bad.Before(good) {
		t.Errorf("a rejection is held until %v and an answer until %v; the rejection "+
			"must be the shorter of the two", bad, good)
	}
}

func TestOneMissIsOneQueryHoweverManyAskAtOnce(t *testing.T) {
	// Many editors starting at once present the same key at the same moment.
	// Without the collapse, that is one query each.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))

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
			got, err := r.Resolve(t.Context(), testKey("sk-live"))
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
	// A client that hangs up must not fail everyone who joined its query.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))
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
		_, err := r.Resolve(context.Background(), testKey("sk-live"))
		first <- err
	}()
	arrived.Wait()

	// The one that joins it and then gives up.
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() {
		_, err := r.Resolve(ctx, testKey("sk-live"))
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

func TestTheCallerDoingTheWorkCanLeaveWithoutFailingTheOthers(t *testing.T) {
	// Everybody waits on the first caller's query, so its client hanging up
	// must not cancel it.
	s := newSource()
	s.set(testKey("sk-live"), resolved("org_1"))
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

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := r.Resolve(ctx, testKey("sk-live"))
		first <- err
	}()
	arrived.Wait()

	joined := make(chan error, 1)
	go func() {
		_, err := r.Resolve(context.Background(), testKey("sk-live"))
		joined <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the caller that gave up got %v, want a cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the caller that gave up is still waiting on the query")
	}
	close(release)
	if err := <-joined; err != nil {
		t.Errorf("the caller that joined was failed by the one that left: %v", err)
	}
}

func TestSweepForgetsWhatIsExpiredAndWhatIsStale(t *testing.T) {
	// An entry is collectable when it has run out, or its generation is over.
	s := newSource()
	s.set(testKey("sk-a"), resolved("org_1"))
	s.reject(testKey("sk-b"), policy.ErrUnknownKey)
	r := newRegistry(t, s, Options{TTL: time.Nanosecond, NegativeTTL: time.Nanosecond})

	if _, err := r.Resolve(t.Context(), testKey("sk-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(t.Context(), testKey("sk-b")); err == nil {
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
	if _, err := r2.Resolve(t.Context(), testKey("sk-a")); err != nil {
		t.Fatal(err)
	}
	if n := r2.cached(); n != 1 {
		t.Fatalf("cached %d entries, want 1", n)
	}
	r2.Forget(anyChange)
	r2.Sweep()
	if n := r2.cached(); n != 0 {
		t.Errorf("cached %d entries after invalidating and sweeping, want 0", n)
	}
}

func TestTheCacheIsKeyedByHashAndNotByTheKeyItself(t *testing.T) {
	// A heap dump must not contain working credentials, so the cache is keyed
	// by hash, as the database is.
	s := newSource()
	s.set(testKey("sk-live-secret-value"), resolved("org_1"))
	r := newRegistry(t, s, Options{TTL: time.Minute})
	if _, err := r.Resolve(t.Context(), testKey("sk-live-secret-value")); err != nil {
		t.Fatal(err)
	}

	for k := range r.keys {
		if k == testKey("sk-live-secret-value") {
			t.Fatal("the presented key is a cache key; a heap dump would hand it out")
		}
		if k != hashOf(testKey("sk-live-secret-value")) {
			t.Errorf("cache key %q is neither the key nor its hash", k)
		}
	}
}

func TestAGatewayDoesNotStartWithoutACatalogue(t *testing.T) {
	// Starting without a load would refuse the first requests after a
	// deployment for a model that exists.
	s := newSource()
	s.loadErr = errors.New("connection refused")
	if _, err := New(t.Context(), s, Options{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("New succeeded with a database it could not read")
	}
}

func TestTheCatalogueIsReplacedWholeOrNotAtAll(t *testing.T) {
	// Guardrails, filters, routers and models name each other. Renewing part
	// of the chain would refuse requests for a link that exists.
	s := newSource()
	s.models = []policy.Model{{Alias: "small"}}
	s.filters = []policy.Filter{{OrgID: "org_1", Alias: "pii"}}
	s.routers = []policy.Router{{OrgID: "org_1", Alias: "cheap-first"}}
	r := newRegistry(t, s, Options{})

	if _, ok := r.Model("", "small"); !ok {
		t.Fatal("the catalogue did not load")
	}

	// The models now read, the filters do not. Nothing may be adopted.
	s.mu.Lock()
	s.models = []policy.Model{{Alias: "small"}, {Alias: "large"}}
	s.loadErr = errors.New("connection refused")
	s.mu.Unlock()

	if err := r.refreshAll(t.Context()); err == nil {
		t.Fatal("refreshAll succeeded with a database it could not read")
	}
	if _, ok := r.Model("", "large"); ok {
		t.Error("half of a failed refresh was adopted")
	}
	if _, ok := r.Filter("org_1", "pii"); !ok {
		t.Error("a failed refresh threw away the filters that were already loaded")
	}
}

func TestAnOrganisationSeesItsOwnModelsAndNoOtherTenants(t *testing.T) {
	s := newSource()
	s.models = []policy.Model{
		{OrgID: "org_1", Alias: "fast", BackendModel: "own"},
		{OrgID: "org_1", Alias: "private"},
		{OrgID: "org_2", Alias: "fast", BackendModel: "theirs"},
	}
	r := newRegistry(t, s, Options{})

	if m, _ := r.Model("org_1", "fast"); m.BackendModel != "own" {
		t.Errorf("org_1 got the %q fast, want its own", m.BackendModel)
	}
	if m, _ := r.Model("org_2", "fast"); m.BackendModel != "theirs" {
		t.Errorf("org_2 got the %q fast, want its own", m.BackendModel)
	}
	if _, ok := r.Model("org_2", "private"); ok {
		t.Error("org_2 can reach org_1's model")
	}
	if _, ok := r.Model("", "private"); ok {
		t.Error("a model is reachable without an organisation")
	}

	var got []string
	for _, m := range r.Models("org_1") {
		got = append(got, m.OrgID+"/"+m.Alias)
	}
	if want := []string{"org_1/fast", "org_1/private"}; !slices.Equal(got, want) {
		t.Errorf("org_1 lists %v, want %v", got, want)
	}
}

func TestALimitedOrganisationUsesOnlyWhatCostsTheDeploymentNothing(t *testing.T) {
	models := func() []policy.Model {
		return []policy.Model{
			{OrgID: "org_new", Alias: "engine", Backends: []string{"http://keera-engine:8000/v1"}},
			{OrgID: "org_new", Alias: "rebound", Backends: []string{"http://10.0.0.5:8000/v1"}},
			{OrgID: "org_new", Alias: "own-key", Backends: []string{"https://api.example.com/v1"}},
			{OrgID: "org_new", Alias: "billed", Provider: "anthropic", BackendModel: "claude-haiku-5-5"},
			{OrgID: "org_paid", Alias: "engine", Backends: []string{"http://keera-engine:8000/v1"}},
		}
	}
	platform := catalog.Platform{"anthropic": {APIKey: "sk-platform"}}

	for _, tc := range []struct {
		prepaid bool
		open    []string
	}{
		{prepaid: true, open: []string{"own-key", "billed"}},
		// Without payments, the deployment's key is billed afterwards, to
		// whoever signed up.
		{prepaid: false, open: []string{"own-key"}},
	} {
		s := newSource()
		s.models = models()
		s.mcp = []policy.MCPServer{
			{OrgID: "org_new", Alias: "inside", URL: "http://tools.svc/mcp"},
			{OrgID: "org_new", Alias: "outside", URL: "https://mcp.example.com/mcp"},
		}
		s.limited = map[string]string{"org_new": "Example Bank"}
		r := newRegistry(t, s, Options{Platform: platform, Prepaid: tc.prepaid})

		for _, m := range r.Models("org_new") {
			if !m.Limited || m.OrgName != "Example Bank" {
				t.Errorf("prepaid=%v: %s is not marked limited for its organisation: %v, %q",
					tc.prepaid, m.Alias, m.Limited, m.OrgName)
			}
			if want := !slices.Contains(tc.open, m.Alias); m.Locked != want {
				t.Errorf("prepaid=%v: %s locked = %v, want %v", tc.prepaid, m.Alias, m.Locked, want)
			}
		}
		if m, _ := r.Model("org_new", "billed"); m.Billing == nil || m.Billing.Credit != tc.prepaid {
			t.Errorf("prepaid=%v: billing = %+v, want credit %v", tc.prepaid, m.Billing, tc.prepaid)
		}
		if m, _ := r.Model("org_paid", "engine"); m.Limited || m.Locked {
			t.Error("an organisation that is not limited has its engine model held back")
		}
		if m, _ := r.MCPServer("org_new", "inside"); !m.Locked {
			t.Error("a limited organisation reaches an MCP server inside the network")
		}
		if m, _ := r.MCPServer("org_new", "outside"); m.Locked || !m.Limited {
			t.Errorf("the public MCP server: locked %v, limited %v; want open and limited",
				m.Locked, m.Limited)
		}
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
	if o.SpendRefresh != 10*time.Second {
		t.Errorf("SpendRefresh = %v, want 10s", o.SpendRefresh)
	}
}

// The tests above only matter while the store is a Source.
var _ Source = (*store.Store)(nil)

func TestATokenThatCannotBeAKeyCostsNoQuery(t *testing.T) {
	// Scanners send tokens that are not shaped like a Keera key. None may
	// reach the database or the cache.
	s := newSource()
	r := newRegistry(t, s, Options{TTL: time.Minute})

	for _, tok := range []string{"sk-live", "Bearer", "keera_sk_short"} {
		if _, err := r.Resolve(t.Context(), tok); !errors.Is(err, policy.ErrUnknownKey) {
			t.Errorf("Resolve(%q) = %v, want ErrUnknownKey", tok, err)
		}
	}
	if n := s.lookups.Load(); n != 0 {
		t.Errorf("%d queries for tokens that cannot be keys, want 0", n)
	}
	if n := r.cached(); n != 0 {
		t.Errorf("%d cache entries for tokens that cannot be keys, want 0", n)
	}
}

func TestKeyQueriesNeverTakeMoreThanTheirShareOfThePool(t *testing.T) {
	// A flood of made-up, well-formed keys must leave the rest of the pool to
	// usage writes and the control plane.
	s := newSource()
	release := make(chan struct{})
	var running, peak atomic.Int32
	s.beforeLookup = func() {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
	}
	r := newRegistry(t, s, Options{TTL: time.Minute})

	const callers = 3 * maxLookups
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() { _, _ = r.Resolve(t.Context(), testKey(fmt.Sprint("flood-", i))) })
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.lookups.Load() < maxLookups && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Give any query that should not have started the chance to.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if p := peak.Load(); p > maxLookups {
		t.Errorf("%d key queries ran at once, want at most %d", p, maxLookups)
	}
	if n := s.lookups.Load(); n != callers {
		t.Errorf("%d queries ran, want %d: a caller waiting for a slot was dropped", n, callers)
	}
}

func TestACallerWhoHangsUpWaitingForASlotGivesItUp(t *testing.T) {
	s := newSource()
	release := make(chan struct{})
	s.beforeLookup = func() { <-release }
	r := newRegistry(t, s, Options{TTL: time.Minute})
	defer close(release)

	for i := range maxLookups {
		go func() { _, _ = r.Resolve(context.Background(), testKey(fmt.Sprint("busy-", i))) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.lookups.Load() < maxLookups && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Resolve(ctx, testKey("late")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resolve = %v, want the caller's own deadline", err)
	}
	if _, loaded := r.inflight.Load(hashOf(testKey("late"))); loaded {
		t.Error("the abandoned query is still marked in flight, so nobody can retry it")
	}
}
