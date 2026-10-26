package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bespinian/keera-gateway/internal/catalog"
	"github.com/bespinian/keera-gateway/internal/policy"
	"github.com/bespinian/keera-gateway/internal/store"
)

func (m *Manager) Suspend(ctx context.Context, sb store.Sandbox) error {
	if sb.State == policy.SandboxSuspended {
		return nil
	}
	// Resuming would start an agent's task again.
	if sb.Purpose == policy.PurposeAgent {
		return Refuse("sandbox %s is an agent's; it runs its task to the end and is not "+
			"suspended. Terminate it to stop it", sb.Name)
	}
	if !sb.State.Running() {
		return Refuse("sandbox %s is %s; only a running sandbox can be suspended",
			sb.Name, sb.State)
	}
	if err := m.driver.Suspend(ctx, refOf(sb)); err != nil {
		return err
	}
	// Stop the clock now, not at the next sweep.
	m.stopClock(ctx, sb.ID)
	return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State: policy.SandboxSuspended, Detail: "suspended; its volume is kept",
	})
}

// Resume brings a suspended or expired sandbox back. An expired one is held to
// the owner's current limits.
func (m *Manager) Resume(ctx context.Context, sb store.Sandbox, limits policy.ResolvedSandbox) error {
	switch {
	case sb.State == policy.SandboxReady || sb.State == policy.SandboxPending:
		return nil
	case sb.State.Final():
		return Refuse("sandbox %s is %s; there is nothing left to resume", sb.Name, sb.State)
	case sb.State == policy.SandboxExpired:
		return m.revive(ctx, sb, limits)
	}
	if err := m.driver.Resume(ctx, refOf(sb)); err != nil {
		if errors.Is(err, ErrNotFound) {
			m.release(ctx, sb)
			return m.gone(ctx, sb)
		}
		return err
	}
	return m.observeResumed(ctx, sb, time.Time{})
}

// gone marks a sandbox the driver no longer has as failed.
func (m *Manager) gone(ctx context.Context, sb store.Sandbox) error {
	m.fail(ctx, sb.ID, errors.New("the sandbox is no longer there; its volume went with it"))
	return Refuse("sandbox %s is no longer there; its volume went with it", sb.Name)
}

// observeResumed reads the state back, since a podman sandbox is already
// running while a pod is still scheduling. expires is zero unless revived.
func (m *Manager) observeResumed(ctx context.Context, sb store.Sandbox, expires time.Time) error {
	status, err := m.driver.Status(ctx, refOf(sb))
	if err != nil {
		// The resume itself worked; the sweep will correct the row.
		m.log.Warn("sandbox: reading a resumed sandbox's state failed",
			"sandbox", sb.ID, "error", err)
		return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
			State: policy.SandboxPending, Detail: "resuming",
		})
	}
	m.observe(ctx, sb.ID, stillStarting(status, expires, time.Now()))
	return nil
}

// revive starts an expired sandbox as if new, but on its volume: a new key,
// credential and default lifetime, under the current guardrail.
func (m *Manager) revive(ctx context.Context, sb store.Sandbox, limits policy.ResolvedSandbox) error {
	if sb.Purpose != policy.PurposeEngineer {
		return Refuse("sandbox %s is an agent's; its task is over, so it is not resumed", sb.Name)
	}
	// The machine keeps its own copy of the class. The current class only
	// decides the lifetime.
	machine := standIn(sb)
	lifetime, err := m.lifetimeClass(ctx, sb)
	if err != nil {
		return err
	}
	if err := limits.Admits(machine); err != nil {
		return Refuse("%v", err)
	}
	expires := time.Now().Add(clampTTL(lifetime, 0, limits)).UTC()

	req := CreateRequest{
		OrgID: sb.OrgID, ProjectID: sb.ProjectID, UserID: sb.UserID, Owner: sb.Owner,
		Name: sb.Name, Class: sb.Class, Purpose: sb.Purpose,
		Repo: sb.Repo, Branch: sb.Branch, AuthorizedKeys: sb.AuthorizedKeys, Limits: limits,
	}
	s, err := m.start(ctx, req, sb, expires)
	if err != nil {
		return err
	}

	if err := m.st.ReviveSandbox(ctx, sb.ID, s.keyID, s.git.ID, expires); err != nil {
		m.undo(ctx, s)
		if errors.Is(err, store.ErrNotFound) {
			return Refuse("sandbox %s is no longer expired; somebody else resumed or "+
				"terminated it", sb.Name)
		}
		return err
	}
	m.changed(s.keyID)

	err = m.driver.Revive(ctx, specFor(refOf(sb), machine, req, s.env, expires))
	if err != nil {
		m.undo(ctx, s)
		if errors.Is(err, ErrNotFound) {
			return m.gone(ctx, sb)
		}
		// Back to expired, with its volume, and without the new key.
		if oerr := m.st.ObserveSandbox(context.WithoutCancel(ctx), sb.ID, store.SandboxObservation{
			State: policy.SandboxExpired, Detail: "resuming failed: " + err.Error(),
		}); oerr != nil {
			m.log.Warn("sandbox: recording a failed resume failed", "sandbox", sb.ID, "error", oerr)
		}
		return fmt.Errorf("resuming sandbox %s: %w", sb.Name, err)
	}
	return m.observeResumed(ctx, sb, expires)
}

// lifetimeClass is the current class, or standIn if it was deleted.
func (m *Manager) lifetimeClass(ctx context.Context, sb store.Sandbox) (policy.SandboxClass, error) {
	class, err := m.st.SandboxClass(ctx, sb.OrgID, sb.Class)
	if errors.Is(err, store.ErrNotFound) {
		return standIn(sb), nil
	}
	return class, err
}

// standIn is the row's own copy of its class. The row has no lifetimes, so it
// gets the defaults; one based on the sandbox's age would grow with each
// extend.
func standIn(sb store.Sandbox) policy.SandboxClass {
	return policy.SandboxClass{
		OrgID: sb.OrgID, Name: sb.Class, Image: sb.Image, Isolation: sb.Isolation,
		CPU: sb.CPU, Memory: sb.Memory, Disk: sb.Disk,
		DefaultTTLSeconds: int(catalog.DefaultSandboxTTL / time.Second),
		MaxTTLSeconds:     int(catalog.DefaultSandboxMaxTTL / time.Second),
	}
}

// stillStarting treats "expired" before the new end as the cluster not having
// caught up.
func stillStarting(status Status, expires, now time.Time) Status {
	if status.State == policy.SandboxExpired && expires.After(now) {
		status.State, status.Detail = policy.SandboxPending, "waiting for the cluster to start it again"
	}
	return status
}

// Extend moves when a sandbox ends. The new lifetime counts from now and is
// clamped like at creation, so repeated extends never add up.
func (m *Manager) Extend(ctx context.Context, sb store.Sandbox, want time.Duration,
	limits policy.ResolvedSandbox,
) (time.Time, error) {
	if sb.State == policy.SandboxExpired {
		return time.Time{}, Refuse("sandbox %s has expired; `keera sandbox resume %s` brings "+
			"it back with a new lifetime", sb.Name, sb.Name)
	}
	if !sb.State.Live() {
		return time.Time{}, Refuse("sandbox %s is %s; its lifetime cannot be extended",
			sb.Name, sb.State)
	}
	class, err := m.lifetimeClass(ctx, sb)
	if err != nil {
		return time.Time{}, err
	}

	until := time.Now().Add(clampTTL(class, want, limits)).UTC()

	if err := m.driver.Extend(ctx, refOf(sb), until); err != nil {
		return time.Time{}, err
	}
	if err := m.st.SetSandboxExpiry(ctx, sb.ID, until); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return time.Time{}, Refuse("sandbox %s ran out of time or was terminated before it "+
				"could be extended; `keera sandbox resume %s` brings an expired one back",
				sb.Name, sb.Name)
		}
		return time.Time{}, err
	}
	return until, nil
}

// Terminate removes a sandbox and revokes its key. The driver goes first, so a
// failure leaves a working sandbox with a working key. A failed revoke only
// logs, as the key expires by itself.
func (m *Manager) Terminate(ctx context.Context, sb store.Sandbox) error {
	return m.terminate(ctx, sb, "terminated")
}

func (m *Manager) terminate(ctx context.Context, sb store.Sandbox, detail string) error {
	if sb.State == policy.SandboxTerminated {
		return nil
	}
	if err := m.driver.Terminate(ctx, refOf(sb)); err != nil {
		return err
	}
	m.release(ctx, sb)
	return m.st.ObserveSandbox(ctx, sb.ID, store.SandboxObservation{
		State: policy.SandboxTerminated, Detail: detail,
	})
}

// Offboarded is what Offboard did.
type Offboarded struct {
	Terminated int `json:"terminated"`
	Suspended  int `json:"suspended"`
	// Failed counts what could not be stopped. The log says why.
	Failed int `json:"failed"`
}

// Offboard stops a disabled person's sandboxes. Their keys are already
// revoked. An agent's, or one not started, is terminated. An engineer's is
// suspended, since its volume may hold work others need. Repository
// credentials are revoked.
func (m *Manager) Offboard(ctx context.Context, userID string) (Offboarded, error) {
	var done Offboarded
	live, err := m.st.ListSandboxes(ctx, store.SandboxQuery{UserID: userID, Limit: 500})
	if err != nil {
		return done, err
	}
	for _, sb := range live {
		keep := sb.Purpose == policy.PurposeEngineer && sb.State != policy.SandboxPending
		var err error
		if !keep {
			if err = m.Terminate(ctx, sb); err == nil {
				done.Terminated++
			}
		} else {
			if sb.State == policy.SandboxReady {
				err = m.Suspend(ctx, sb)
			}
			if err == nil {
				m.revokeGit(ctx, sb)
				// An expired or failed one is kept too, but is not suspended.
				if sb.State == policy.SandboxReady || sb.State == policy.SandboxSuspended {
					done.Suspended++
				}
			}
		}
		if err != nil {
			done.Failed++
			m.log.Warn("sandbox: stopping a disabled person's sandbox failed",
				"sandbox", sb.ID, "name", sb.Name, "error", err)
		}
	}
	return done, nil
}
