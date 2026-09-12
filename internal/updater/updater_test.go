package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckValidatesGitHubAndPreservesStateOnFailure(t *testing.T) {
	current := strings.Repeat("a", 40)
	latest := strings.Repeat("b", 40)
	body := `{"sha":"` + latest + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer server.Close()
	m := &Manager{state: State{Current: current}, client: server.Client(), endpoint: server.URL}
	m.Check(context.Background())
	if !m.View().Available || m.View().Latest != latest {
		t.Fatal("new revision not detected")
	}
	m.state.Checked = m.state.Checked.Add(-2 * 60 * 1000000000)
	body = `{"sha":"not-a-revision"}`
	m.Check(context.Background())
	if m.View().Error == "" || m.View().Latest != latest {
		t.Fatal("invalid response accepted or previous state lost")
	}
}
func TestInstallRejectsDisabledOrUnverifiedTarget(t *testing.T) {
	m := &Manager{state: State{Current: strings.Repeat("a", 40), Latest: strings.Repeat("b", 40), Available: true}}
	if m.Install(context.Background(), m.state.Latest) == nil {
		t.Fatal("disabled updater accepted install")
	}
	m.state.Enabled = true
	if m.Install(context.Background(), "untrusted") == nil {
		t.Fatal("unverified revision accepted")
	}
}
