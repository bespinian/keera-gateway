package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

// Sweep reconciles every live row with the driver, ends expired ones,
// suspends idle ones and updates the running time. On podman it is the only
// thing that enforces expiry.
func (m *Manager) Sweep(ctx context.Context, now time.Time) {
	// Accounting first, so a sandbox ended below pays for its last interval.
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

// endExpired terminates an agent's sandbox whose time ran out. An engineer's
// is suspended and marked expired, because its volume holds work. Its key and
// repository credential are revoked; resuming gives new ones.
func (m *Manager) endExpired(ctx context.Context, sb store.Sandbox) {
	// Charge up to now first: once the row says expired, the clock stops.
	m.stopClock(ctx, sb.ID)
	// The row may be stale: if its owner extended it since, it lives on.
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
	// Nothing legitimate uses the key while the sandbox is stopped.
	m.release(ctx, sb)
}

// removeAgent terminates an agent's sandbox and leaves why on its row. If the
// driver fails, the next sweep tries again.
func (m *Manager) removeAgent(ctx context.Context, sb store.Sandbox, why string) {
	if err := m.terminate(ctx, sb, why); err != nil {
		m.log.Warn("sandbox: removing an agent sandbox failed",
			"sandbox", sb.ID, "why", why, "error", err)
	}
}

// failed records a sandbox that stopped for good, and revokes its key and
// repository credential. An agent's is removed from the driver. An engineer's
// keeps its volume, which may hold work, until somebody terminates it; until
// then it counts towards the quota.
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

// reconcile writes back what the driver says about one sandbox. An expired or
// failed one is only checked for having vanished: to a driver, an expired one
// looks suspended.
func (m *Manager) reconcile(ctx context.Context, sb store.Sandbox, now time.Time) {
	status, err := m.driver.Status(ctx, refOf(sb))
	settled := sb.State == policy.SandboxExpired || sb.State == policy.SandboxFailed
	if settled && !errors.Is(err, ErrNotFound) {
		// An earlier sweep failed to remove this expired agent sandbox.
		if err == nil && sb.State == policy.SandboxExpired && sb.Purpose == policy.PurposeAgent {
			m.removeAgent(ctx, sb, "its lifetime ran out")
		}
		return
	}
	switch {
	case errors.Is(err, ErrNotFound):
		// Gone: usually the controller expired it, or someone removed it.
		m.release(ctx, sb)
		m.observeFrom(ctx, sb, Status{
			Ref: refOf(sb), State: policy.SandboxTerminated,
			Detail: "no longer there",
		})
		return
	case err != nil:
		// An unreachable driver is not a dead sandbox, so leave the row alone.
		m.log.Warn("sandbox: reading a sandbox's state failed", "sandbox", sb.ID, "error", err)
		return
	}
	// An agent that exited cleanly has done its task. Agents are never
	// suspended, so this is not a podman suspend.
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
		// The controller saw the end first. Recorded alone, it would keep its
		// key and credential, since the sweep skips what is expired.
		m.endExpired(ctx, sb)
		return
	}
	m.observeFrom(ctx, sb, status)

	// Idle suspension. The row must say ready too: a sandbox that has only
	// now come back is not idle.
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

// idleSince is the latest of the last connection and the return to ready.
func idleSince(sb store.Sandbox) time.Time {
	since := sb.CreatedAt
	for _, t := range []*time.Time{sb.ReadyAt, sb.LastActiveAt} {
		if t != nil && t.After(since) {
			since = *t
		}
	}
	return since
}

// reconcilePools makes the warm pools match the catalogue on every sweep, so
// edits apply without a restart.
func (m *Manager) reconcilePools(ctx context.Context) {
	pooler, ok := m.driver.(Pooler)
	if !ok {
		return
	}
	if !m.driver.Capabilities().Warm {
		// Leftover pools would hold their resources for ever. Turning pools
		// off takes a restart, so one try is enough.
		m.pruneOnce.Do(func() {
			if err := pooler.PrunePools(ctx, nil); err != nil {
				m.log.Warn("sandbox: removing the warm pools failed; "+
					"they keep holding their CPU and memory", "error", err)
			}
		})
		return
	}
	// A class with no warm pool, or of an organisation that has not paid, is
	// not returned, so PrunePools deletes its pool.
	classes, err := m.st.WarmSandboxClasses(ctx)
	if err != nil {
		m.log.Warn("sandbox: reading the catalogue to reconcile warm pools failed", "error", err)
		return
	}
	keep := make(map[string]bool, len(classes))
	for _, c := range classes {
		keep[poolKey(c.OrgID, c.Name)] = true
		if err := pooler.EnsurePool(ctx, c); err != nil {
			// Not fatal: an existing pool keeps serving, and the next sweep
			// tries again.
			m.log.Warn("sandbox: reconciling a warm pool failed",
				"org", c.OrgID, "class", c.Name, "warm", c.Warm, "error", err)
		}
	}
	if err := pooler.PrunePools(ctx, keep); err != nil {
		m.log.Warn("sandbox: removing the warm pools of deleted classes failed", "error", err)
	}
}
