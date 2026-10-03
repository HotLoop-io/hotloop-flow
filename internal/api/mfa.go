package api

import (
	"errors"
	"net/http"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/mfa"
)

// PermAuthAdmin lets an account turn off somebody else's two-factor sign-in,
// for the day a phone goes in the coolant tank.
const PermAuthAdmin = "auth.admin"

func (s *Server) mfaRoutes() {
	s.mux.Handle("GET "+s.path("/auth/mfa"), s.signedIn(s.handleMFAStatus))
	s.mux.Handle("POST "+s.path("/auth/mfa/setup"), s.signedIn(s.handleMFASetup))
	s.mux.Handle("POST "+s.path("/auth/mfa/confirm"), s.signedIn(s.handleMFAConfirm))
	s.mux.Handle("POST "+s.path("/auth/mfa/disable"), s.signedIn(s.handleMFADisable))
	s.mux.Handle("POST "+s.path("/auth/mfa/reset"), s.auth(PermAuthAdmin, s.handleMFAReset))
}

// signedIn requires a person's session: two-factor sign-in belongs to a user,
// so an API token can't set it up, and with authentication off there is no
// sign-in to protect.
func (s *Server) signedIn(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.MFA == nil || !s.deps.Config.Auth.Enabled {
			writeError(w, http.StatusConflict, "authentication is off, so there is no sign-in for a second factor to protect")
			return
		}
		tok := bearerToken(r)
		user, ok := s.tokens.lookup(tok)
		if tok == "" || !ok {
			writeError(w, http.StatusUnauthorized, "two-factor settings belong to a signed-in user; sign in first")
			return
		}
		h(w, r.WithContext(withUser(r, user.Username)))
	})
}

func (s *Server) handleMFAStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.deps.MFA.Enabled(requestUser(r))})
}

// handleMFASetup starts enrolling and hands back what the phone needs: the
// secret to type, the otpauth:// link, and the QR code as rows of modules for
// the editor to draw.
func (s *Server) handleMFASetup(w http.ResponseWriter, r *http.Request) {
	user := requestUser(r)
	secret, err := s.deps.MFA.Begin(user)
	if errors.Is(err, mfa.ErrAlreadyEnrolled) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	uri := mfa.URI(user, secret)
	rows, err := mfa.QR(uri)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret, "uri": uri, "qr": rows})
}

type mfaCode struct {
	Code     string `json:"code"`
	Username string `json:"username"`
}

func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request) {
	var req mfaCode
	if err := readJSON(r, 4096, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := requestUser(r)
	switch err := s.deps.MFA.Confirm(user, req.Code); {
	case errors.Is(err, mfa.ErrNoPending):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, mfa.ErrBadCode):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, audit.MFAEnabled, user, nil)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true})
}

// handleMFADisable turns it off, which takes a current code: a session left
// open on a shared terminal mustn't be enough to strip the second factor off
// an account.
func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request) {
	var req mfaCode
	if err := readJSON(r, 4096, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := requestUser(r)
	if err := s.deps.MFA.Verify(user, req.Code); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, mfa.ErrNotEnrolled) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	if err := s.deps.MFA.Disable(user); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, audit.MFADisabled, user, nil)
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

// handleMFAReset turns off somebody else's two-factor sign-in, for a lost
// phone. It takes auth.admin and it lands in the audit trail with who did it
// to whom.
func (s *Server) handleMFAReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.MFA == nil {
		writeError(w, http.StatusConflict, "two-factor sign-in is not available")
		return
	}
	var req mfaCode
	if err := readJSON(r, 4096, &req); err != nil || req.Username == "" {
		writeError(w, http.StatusBadRequest, `send {"username": "..."}`)
		return
	}
	if err := s.deps.MFA.Disable(req.Username); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mfa.ErrNotEnrolled) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	s.record(r, audit.MFAReset, requestUser(r), map[string]any{"for": req.Username})
	writeJSON(w, http.StatusOK, map[string]any{"username": req.Username, "enabled": false})
}

// secondFactor checks the code for a user whose password was right. It reports
// whether sign-in may go ahead, and answers the request itself when it may
// not. failed is true when a code was given and was wrong, which counts
// against the sign-in limits; a missing code doesn't, because the editor
// always sends the password first and asks for the code when told to.
func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request, user, code string) (ok, failed bool) {
	if s.deps.MFA == nil || !s.deps.MFA.Enabled(user) {
		return true, false
	}
	if code == "" {
		// Not a failure: the editor sends the password first and asks for
		// the code when told to.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "enter the six-digit code from your authenticator app",
			"mfa":   "required",
		})
		return false, false
	}
	if err := s.deps.MFA.Verify(user, code); err != nil {
		s.log.Warn("failed two-factor code", "username", user, "remote", r.RemoteAddr)
		s.record(r, audit.MFAFailed, user, map[string]any{"reason": err.Error()})
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error(), "mfa": "required"})
		return false, true
	}
	return true, false
}

// strike counts a failed sign-in against the limits, and says so in the audit
// trail when it just locked something.
func (s *Server) strike(r *http.Request, user, addr string) {
	if s.limiter.fail(user, addr) {
		s.log.Warn("sign-in locked after repeated failures", "username", user, "remote", r.RemoteAddr)
		s.record(r, audit.LoginLocked, user, map[string]any{"forSeconds": int(s.limiter.lockFor.Seconds())})
	}
}
