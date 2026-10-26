package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Sweep reconciles every live row with the driver, ends the expired ones,
// suspends idle ones and updates the running time.
//
// On Kubernetes the controller also enforces expiry; on podman this is the
// only thing that does.
func (m *Manager) Sweep(ctx context.Context, now time.Time) {
	// Accounting first, so a sandbox ended below is charged for its last
	// interval.
	if _, err := m.st.AccountSandboxes(ctx, now); err != nil {
		m.log.Warn("sandbox: accounting for running time failed", "error", err)
	}

	expired, err := m.st.SandboxesPastExpiry(ctx, now, 100)
	if err != nil {
		m.log.Warn("sandbox: reading expired sandboxes failed", "error", err)
	}
	for _, sb := range expired {
		m.endExpired(ctx, sb)
	}

	m.reconcilePools(ctx)

	const page = 500
	for after := ""; ; {
		live, err := m.st.LiveSandboxes(ctx, after, page)
		if err != nil {
			m.log.Warn("sandbox: reading live sandboxes failed", "error", err)
			return
		}
		for _, sb := range live {
			if ctx.Err() != nil {
				return
			}
			m.reconcile(ctx, sb, now)
		}
		if len(live) < page {
			return
		}
		after = live[len(live)-1].ID
	}
}

// endExpired ends a sandbox whose time ran out.
//
// An agent's is terminated: the task is over and its result is a branch. An
// engineer's is suspended and marked expired, because its volume holds work
// in progress. Its key and repository credential are revoked; resuming it
// gives it new ones.
func (m *Manager) endExpired(ctx context.Context, sb store.Sandbox) {
	// Charged up to now first: once the row says expired, the clock stops.
	m.stopClock(ctx, sb.ID)
	// The row was read a while ago. If its owner extended it since, it lives
	// on; if they extend it after this, they are refused.
	detail := "its lifetime ran out; resuming it brings it back, terminating it frees the volume"
	if sb.Purpose == policy.PurposeAgent {
		detail = "its lifetime ran out; it is being removed"
	}
	if err := m.st.ExpireSandbox(ctx, sb, detail); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("sandbox: recording an expiry failed", "sandbox", sb.ID, "error", err)
		}
		return
	}
	if sb.Purpose == policy.PurposeAgent {
		m.removeAgent(ctx, sb, "its lifetime ran out")
		return
	}
	if sb.State.Running() {
		if err := m.driver.Suspend(ctx, refOf(sb)); err != nil && !errors.Is(err, ErrNotFound) {
			m.log.Warn("sandbox: suspending an expired sandbox failed",
				"sandbox", sb.ID, "error", err)
		}
	}
	// The key goes now, not at termination: nothing legitimate uses it while
	// the sandbox is stopped.
	m.release(ctx, sb)
}

// removeAgent terminates an agent's sandbox, whose task is done or whose
// time ran out, and leaves why on its row. If the driver fails, the row stays
// as it is, and the next sweep tries again.
func (m *Manager) removeAgent(ctx context.Context, sb store.Sandbox, why string) {
	if err := m.terminate(ctx, sb, why); err != nil {
		m.log.Warn("sandbox: removing an agent sandbox failed",
			"sandbox", sb.ID, "why", why, "error", err)
	}
}

// failed records a sandbox that stopped by itself and will not come back,
// and revokes its key and repository credential.
//
// An agent's is removed from the driver: its result is the branch it pushed,
// and nothing sweeps its row again. An engineer's keeps its volume, which may
// hold work, until somebody terminates it. Until then it counts towards the
// quota and keeps its name, and the sweep notices if its volume vanishes.
func (m *Manager) failed(ctx context.Context, sb store.Sandbox, status Status) {
	if sb.Purpose == policy.PurposeAgent {
		if err := m.driver.Terminate(ctx, refOf(sb)); err != nil && !errors.Is(err, ErrNotFound) {
			// The row stays live, so the next sweep tries again.
			m.log.Warn("sandbox: removing a failed agent sandbox failed",
				"sandbox", sb.ID, "error", err)
			return
		}
	}
	m.release(ctx, sb)
	m.observe(ctx, sb.ID, status)
}

// reconcile writes back what the driver says about one sandbox.
//
// An expired or failed sandbox is only checked for having vanished. Its state
// is settled: to a driver an expired one looks suspended, and a failed one
// has already given back its key.
func (m *Manager) reconcile(ctx context.Context, sb store.Sandbox, now time.Time) {
	status, err := m.driver.Status(ctx, refOf(sb))
	settled := sb.State == policy.SandboxExpired || sb.State == policy.SandboxFailed
	if settled && !errors.Is(err, ErrNotFound) {
		// An agent's is not kept, so an expired one still there is one an
		// earlier sweep failed to remove.
		if err == nil && sb.State == policy.SandboxExpired && sb.Purpose == policy.PurposeAgent {
			m.removeAgent(ctx, sb, "its lifetime ran out")
		}
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		// Gone. On Kubernetes that is usually the controller expiring it;
		// otherwise somebody removed it by hand. Either way, record the end.
		m.release(ctx, sb)
		m.observeFrom(ctx, sb, Status{
			Ref: refOf(sb), State: policy.SandboxTerminated,
			Detail: "no longer there",
		})
		return
	case err != nil:
		// An unreachable driver is not a dead sandbox. Leave the row alone, so
		// an API server hiccup does not mark everyone's machines as gone.
		m.log.Warn("sandbox: reading a sandbox's state failed", "sandbox", sb.ID, "error", err)
		return
	}
	// An agent that exited cleanly has done its task. It is never suspended,
	// so on podman a clean stop cannot be mistaken for one.
	if status.Exited && sb.Purpose == policy.PurposeAgent {
		m.removeAgent(ctx, sb, "the agent finished its task")
		return
	}
	if status.State == policy.SandboxFailed {
		m.failed(ctx, sb, status)
		return
	}
	if sb.ExpiresAt != nil {
		status = stillStarting(status, *sb.ExpiresAt, now)
	}
	if status.State == policy.SandboxExpired {
		// The controller saw the lifetime end before the expiry sweep did,
		// which reads a page at a time. Recorded alone, it would keep its key
		// and its repository credential, and the sweep skips what is expired.
		m.endExpired(ctx, sb)
		return
	}
	m.observeFrom(ctx, sb, status)

	// Idle suspension, for an engineer's sandbox. It has to have been ready
	// when the row was read too: a sandbox that has only now come back is
	// not idle from before, and its row does not say so yet.
	if m.opts.IdleSuspend > 0 && status.State == policy.SandboxReady &&
		sb.State == policy.SandboxReady && sb.Purpose == policy.PurposeEngineer &&
		idleSince(sb).Add(m.opts.IdleSuspend).Before(now) {
		m.log.Info("sandbox: suspending an idle sandbox",
			"sandbox", sb.ID, "name", sb.Name, "idle_for", now.Sub(idleSince(sb)).Round(time.Minute))
		if err := m.Suspend(ctx, sb); err != nil {
			m.log.Warn("sandbox: suspending an idle sandbox failed", "sandbox", sb.ID, "error", err)
		}
	}
}

// idleSince is when a sandbox counts as idle from: the last time somebody
// was connected or it came back to ready, whichever is latest.
func idleSince(sb store.Sandbox) time.Time {
	since := sb.CreatedAt
	for _, t := range []*time.Time{sb.ReadyAt, sb.LastActiveAt} {
		if t != nil && t.After(since) {
			since = *t
		}
	}
	return since
}

// reconcilePools makes the warm pools match the catalogue. It runs on every
// sweep, so catalogue edits apply without a restart, a deleted pool comes back
// and the pool of a deleted class goes. Only Kubernetes has warm pools.
func (m *Manager) reconcilePools(ctx context.Context) {
	pooler, ok := m.driver.(Pooler)
	if !ok {
		return
	}
	if !m.driver.Capabilities().Warm {
		// Pools left from when warm pools were on would hold their CPU and
		// memory for ever. Turning them off takes a restart, so one try is
		// enough.
		m.pruneOnce.Do(func() {
			if err := pooler.PrunePools(ctx, nil); err != nil {
				m.log.Warn("sandbox: removing the warm pools failed; "+
					"they keep holding their CPU and memory", "error", err)
			}
		})
		return
	}
	// A class with no warm pool, or of an organisation that has not paid yet,
	// is not returned, so PrunePools deletes any pool it once had.
	classes, err := m.st.WarmSandboxClasses(ctx)
	if err != nil {
		m.log.Warn("sandbox: reading the catalogue to reconcile warm pools failed", "error", err)
		return
	}
	keep := make(map[string]bool, len(classes))
	for _, c := range classes {
		keep[poolKey(c.OrgID, c.Name)] = true
		if err := pooler.EnsurePool(ctx, c); err != nil {
			// Not fatal: a pool that already exists keeps serving claims, and
			// the next sweep tries again. Create does not fall back to a cold
			// start, so a class whose pool was never made cannot be claimed.
			m.log.Warn("sandbox: reconciling a warm pool failed",
				"org", c.OrgID, "class", c.Name, "warm", c.Warm, "error", err)
		}
	}
	if err := pooler.PrunePools(ctx, keep); err != nil {
		m.log.Warn("sandbox: removing the warm pools of deleted classes failed", "error", err)
	}
}
