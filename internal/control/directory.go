package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bespinian/keera-gateway/internal/authn"
	"github.com/bespinian/keera-gateway/internal/store"
)

// A role comes from the directory at sign-in, but a session or a `keera login`
// token lasts 30 days. So the gateway keeps the refresh token each
// sign-in returns and asks the directory again every
// authn.DirectoryCheckEvery. Someone removed from it loses access then, and a
// changed group changes their role.

// directoryCheckTimeout bounds the one request that pays for a check.
const directoryCheckTimeout = 10 * time.Second

// RefreshTokenName is what an identity's refresh token is sealed under, so a
// ciphertext copied to another identity does not open.
func RefreshTokenName(externalID string) string { return "refresh-token:" + externalID }

// directoryOf is the identity provider that vouches for a person, or nil for
// an account no directory knows: the operator key's stand-in, or a passkey
// account.
func (s *Server) directoryOf(user store.User) *authn.OIDC {
	name, _, ok := strings.Cut(user.ExternalID, ":")
	if !ok {
		return nil
	}
	return s.opts.Providers.ByName(name)
}

// sealRefreshToken seals an identity's refresh token, or returns nil for
// none, which keeps the one stored.
func (s *Server) sealRefreshToken(externalID, refreshToken string) []byte {
	if refreshToken == "" {
		return nil
	}
	return s.opts.Secrets.Seal(RefreshTokenName(externalID), refreshToken)
}

// rememberDirectory keeps the sealed refresh token a sign-in returned. A
// failure is logged, not shown: the sign-in itself worked.
func (s *Server) rememberDirectory(r *http.Request, user store.User, sealed []byte) {
	if err := s.st.RecordDirectoryCheck(r.Context(), user.ExternalID, sealed); err != nil {
		s.log.Error("storing a refresh token", "user", user.ID, "error", err)
	}
}

// checkDirectory asks the person's directory again when a check is due, and
// applies the answer. It returns the user as they now stand, and false when
// the directory no longer vouches for them, after ending all their sign-ins.
// A provider that cannot be reached keeps the person signed in: signing
// everybody out during an outage would be worse, and the next check asks again.
func (s *Server) checkDirectory(r *http.Request, user store.User, d store.Directory) (store.User, bool) {
	now := time.Now()
	if !d.Due(now, authn.DirectoryCheckEvery) {
		return user, true
	}
	provider := s.directoryOf(user)
	if provider == nil {
		return user, true
	}
	// Not the request's context: a client that hangs up must not abandon a
	// refresh the provider has already rotated.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), directoryCheckTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	sealed, claimed, err := s.st.ClaimDirectoryCheck(ctx, user.ExternalID,
		now.Add(-authn.DirectoryCheckEvery))
	if err != nil || !claimed {
		// Another request is asking right now, or just did.
		return user, true
	}
	refreshToken, err := s.opts.Secrets.Open(RefreshTokenName(user.ExternalID), sealed)
	if err != nil {
		// Usually KEERA_SECRET_KEY changed. Kept, the sign-in would never be
		// checked again, so it ends, and signing in again stores a new token.
		return user, s.directoryFailed(r, user, fmt.Errorf(
			"%w: its refresh token cannot be decrypted with KEERA_SECRET_KEY: %w",
			authn.ErrDirectoryRefused, err))
	}
	identity, fresh, err := provider.Recheck(ctx, refreshToken)
	if err == nil && fresh && identity.ExternalID() != user.ExternalID {
		err = fmt.Errorf("%w: it answered for a different subject", authn.ErrDirectoryRefused)
	}
	if err != nil {
		return user, s.directoryFailed(r, user, err)
	}
	sealed = s.sealRefreshToken(user.ExternalID, identity.RefreshToken)
	if err := s.st.RecordDirectoryCheck(ctx, user.ExternalID, sealed); err != nil {
		s.log.Error("storing a refreshed token", "user", user.ID, "error", err)
	}
	if !fresh {
		return user, true
	}
	if identity.Name != user.Name {
		if err := s.st.SetIdentityName(ctx, user.ExternalID, identity.Name); err != nil {
			s.log.Error("storing a name from the directory", "user", user.ID, "error", err)
		} else {
			user.Name = identity.Name
		}
	}
	was := user.Role
	role := provider.Mapping().RoleFor(identity.Email, identity.Groups)
	if err := s.syncRole(r, &user, role); err != nil {
		s.log.Error("applying a role from the directory", "user", user.ID, "error", err)
		return user, true
	}
	if err := s.syncOtherRoles(r, user, role); err != nil {
		s.log.Error("applying a role from the directory", "user", user.ID, "error", err)
	}
	if user.Role != was {
		s.auditf(r, actorOf(user),
			user.OrgID, "auth.directory_check", "user", user.ID,
			map[string]any{"role": user.Role, "role_was": was})
	}
	return user, true
}

// directoryFailed handles a check that did not succeed. It reports whether
// the person stays signed in.
func (s *Server) directoryFailed(r *http.Request, user store.User, err error) bool {
	if !errors.Is(err, authn.ErrDirectoryRefused) {
		s.log.Warn("could not ask the directory again; keeping the sign-in",
			"user", user.ID, "error", err)
		return true
	}
	if err := s.st.EndDirectoryLink(r.Context(), user.ExternalID); err != nil {
		s.log.Error("ending the sign-ins of someone the directory refused",
			"user", user.ID, "error", err)
		// Refuse this request all the same: the directory has spoken.
		return false
	}
	s.log.Info("the directory no longer vouches for someone; their sign-ins ended",
		"user", user.ID, "reason", err)
	s.auditf(r, actorOf(user),
		user.OrgID, "auth.directory_check", "user", user.ID,
		map[string]any{"signed_out": true})
	return false
}
