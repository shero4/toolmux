package web

import (
	"github.com/shero4/toolmux/internal/store"
	"net/http"
)

type settingsPage struct {
	Error      string
	Monitoring store.OperationSettings
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	v, err := s.store.OperationSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, 200, "settings", "settings", "Settings", settingsPage{Monitoring: v})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	if r.FormValue("password") != r.FormValue("confirm") {
		s.render(w, r, 400, "settings", "settings", "Settings", settingsPage{Error: "Passwords do not match."})
		return
	}
	if err := s.store.ChangeUserPassword(r.Context(), currentUser(r).ID, r.FormValue("current"), r.FormValue("password")); err != nil {
		s.render(w, r, 400, "settings", "settings", "Settings", settingsPage{Error: "Check your current password and use a new password of 12–72 bytes."})
		return
	}
	s.session(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
