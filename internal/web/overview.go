package web

import (
	"net/http"
	"time"

	"github.com/shero4/toolmux/internal/store"
)

type overviewPage struct {
	Agents, ActiveAgents            int
	Connections, HealthyConnections int
	Tools                           int
	Stats                           store.AuditStats
	Attention                       []store.Connection
	Recent                          []store.AuditEvent
	Empty                           bool
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	connections, err := s.store.ListConnections(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	_, _, tools, err := s.store.Summary(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	stats, err := s.store.AuditStatsSince(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	recent, err := s.store.ListAudit(ctx, 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	page := overviewPage{Agents: len(agents), Connections: len(connections), Tools: tools, Stats: stats, Recent: recent, Empty: len(agents) == 0 && len(connections) == 0}
	for _, agent := range agents {
		if agent.Status == "active" {
			page.ActiveAgents++
		}
	}
	for _, connection := range connections {
		switch connection.Status {
		case "connected":
			page.HealthyConnections++
		case "disabled":
		default:
			page.Attention = append(page.Attention, connection)
		}
	}
	s.render(w, r, http.StatusOK, "overview", "overview", "Overview", page)
}
