package web

import (
	"errors"
	"net/http"

	"github.com/shero4/toolmux/internal/store"
)

type usersPage struct {
	Users []store.User
	Error string
}

func (s *Server) userPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, status, "users", "users", "Users", usersPage{Users: users, Error: message})
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) { s.userPage(w, r, 200, "") }
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	if r.FormValue("password") != r.FormValue("confirm") {
		s.userPage(w, r, 400, "Passwords do not match.")
		return
	}
	err := s.store.CreateUser(r.Context(), currentUser(r).ID, r.FormValue("username"), r.FormValue("password"), r.FormValue("role"))
	if err != nil {
		s.userPage(w, r, 400, "Could not create user. Use a unique username, a valid role, and a 12–72 byte password.")
		return
	}
	s.redirect(w, r, "/users", flash{Kind: "ok", Message: "User created. They can sign in and change their password in Settings."})
}
func (s *Server) setUserAccess(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	err := s.store.SetUserAccess(r.Context(), currentUser(r).ID, r.PathValue("id"), r.FormValue("role"), r.FormValue("enabled") == "on")
	if errors.Is(err, store.ErrLastAdmin) {
		s.userPage(w, r, 400, "Keep at least one active administrator. Promote another user before changing this account.")
		return
	}
	if err != nil {
		s.userPage(w, r, 400, "Could not update user access.")
		return
	}
	if r.PathValue("id") == currentUser(r).ID {
		s.session(w, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.redirect(w, r, "/users", flash{Kind: "ok", Message: "Access updated. This user's previous sessions have been signed out."})
}
