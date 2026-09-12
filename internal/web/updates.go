package web

import (
	"context"
	"encoding/json"
	"net/http"
)

func (s *Server) RunUpdates(ctx context.Context) { s.updates.Run(ctx) }
func (s *Server) updatesPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, 200, "updates", "updates", "Updates", s.updates.View())
}
func (s *Server) updatesStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.updates.View())
}
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid request", 400)
		return
	}
	if err := s.updates.Install(r.Context(), r.FormValue("revision")); err != nil {
		s.redirect(w, r, "/settings/updates", errorFlash(err.Error()))
		return
	}
	http.Redirect(w, r, "/settings/updates", http.StatusSeeOther)
}
