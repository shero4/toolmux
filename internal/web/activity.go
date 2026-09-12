package web

import (
	"net/http"
	"strings"

	"github.com/shero4/toolmux/internal/store"
)

type activityPage struct {
	Events                   []store.AuditEvent
	Agents                   []store.Agent
	Query, Decision, AgentID string
	Filtered                 bool
	Pager                    pager
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	const pageSize = 50
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	page := activityPage{Agents: agents, Query: strings.TrimSpace(r.URL.Query().Get("q")), Decision: r.URL.Query().Get("decision"), AgentID: r.URL.Query().Get("agent")}
	page.Filtered = page.Query != "" || page.Decision != "" || page.AgentID != ""
	requested := pageNumber(r)
	events, total, err := s.store.SearchAudit(r.Context(), page.Query, page.Decision, page.AgentID, pageSize, (requested-1)*pageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	page.Pager = newPager(r, requested, total, pageSize)
	if page.Pager.Page != requested {
		if events, _, err = s.store.SearchAudit(r.Context(), page.Query, page.Decision, page.AgentID, pageSize, (page.Pager.Page-1)*pageSize); err != nil {
			s.fail(w, err)
			return
		}
	}
	page.Events = events
	s.render(w, r, http.StatusOK, "activity", "activity", "Activity", page)
}
