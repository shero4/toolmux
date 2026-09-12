package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/shero4/toolmux/internal/store"
)

type userContextKey struct{}

func currentUser(r *http.Request) store.User {
	u, _ := r.Context().Value(userContextKey{}).(store.User)
	return u
}

// Browser permissions are enforced before any page or action handler runs.
func permitted(u store.User, r *http.Request) bool {
	if !u.Enabled {
		return false
	}
	if r.URL.Path == "/users" || strings.HasPrefix(r.URL.Path, "/users/") {
		return u.IsAdmin()
	}
	if strings.HasPrefix(r.URL.Path, "/settings/") && r.URL.Path != "/settings/password" || r.URL.Path == "/auth/codex" && r.Method == "POST" {
		return u.IsAdmin()
	}
	if strings.HasSuffix(r.URL.Path, "/rotate") {
		return u.CanManage()
	}
	if r.URL.Path == "/logout" || r.URL.Path == "/settings/password" {
		return r.Method == http.MethodPost
	}
	if u.CanManage() {
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.HasSuffix(r.URL.Path, "/new") || r.URL.Path == "/agents/discover" || r.URL.Path == "/oauth/callback" {
		return false
	}
	return u.Role == "viewer"
}

const sessionCookie = "toolmux_session"

type authPage struct {
	Setup bool
	Error string
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || r.URL.Path == "/mcp" || r.URL.Path == "/admin/mcp" || r.URL.Path == "/oauth/callback" || strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		configured, err := s.store.AdminConfigured(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if !configured {
			if r.URL.Path != "/setup" {
				http.Redirect(w, r, "/setup", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/setup" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		if err == nil {
			user, checkErr := s.store.SessionUser(r.Context(), cookie.Value)
			if checkErr != nil && !errors.Is(checkErr, store.ErrInvalidLogin) {
				s.fail(w, checkErr)
				return
			}
			if checkErr == nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey{}, user))
				if !permitted(user, r) {
					s.render(w, r, http.StatusForbidden, "error", "", "Access denied", errorPage{Title: "Access denied", Message: "Your role does not permit this action. Contact an administrator to change your access."})
					return
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) authForm(w http.ResponseWriter, r *http.Request) {
	setup := r.URL.Path == "/setup"
	title := "Sign in"
	if setup {
		title = "Set up Toolmux"
	}
	s.render(w, r, http.StatusOK, "auth", "auth", title, authPage{Setup: setup})
}

func (s *Server) submitAuth(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	address, _, _ := net.SplitHostPort(r.RemoteAddr)
	allowed, err := s.store.AllowLogin(r.Context(), address)
	if err != nil {
		s.fail(w, err)
		return
	}
	setup := r.URL.Path == "/setup"
	reject := func(status int, message string) {
		s.render(w, r, status, "auth", "auth", "Sign in", authPage{Setup: setup, Error: message})
	}
	if !allowed {
		w.Header().Set("Retry-After", "900")
		reject(http.StatusTooManyRequests, "Too many attempts. Try again in 15 minutes.")
		return
	}
	if setup {
		if r.FormValue("password") != r.FormValue("confirm") {
			reject(400, "Passwords do not match.")
			return
		}
		err = s.store.CreateAdmin(r.Context(), r.FormValue("username"), r.FormValue("password"))
		if err != nil {
			reject(400, "Could not create administrator. Use a username and a 12–72 byte password; setup may already be complete.")
			return
		}
	}
	user, err := s.store.AuthenticateUser(r.Context(), r.FormValue("username"), r.FormValue("password"))
	if err != nil {
		reject(401, "Invalid username or password.")
		return
	}
	token, err := s.store.NewAdminSession(r.Context(), user.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.session(w, token, 12*60*60)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) session(w http.ResponseWriter, token string, maxAge int) {
	// Lax permits the top-level OAuth callback; every administrative mutation
	// remains a same-origin POST.
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.baseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err = s.store.DeleteAdminSession(r.Context(), cookie.Value); err != nil {
			s.fail(w, err)
			return
		}
	}
	s.session(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
