package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewPagerPreservesFiltersAndClampsPage(t *testing.T) {
	request := httptest.NewRequest("GET", "/tools?q=stripe&kind=mcp&page=99", nil)
	got := newPager(request, 99, 51, 25)

	if got.Page != 3 || got.Pages != 3 || got.From != 51 || got.To != 51 {
		t.Fatalf("unexpected pager: %+v", got)
	}
	if got.HasNext || !got.HasPrevious {
		t.Fatalf("unexpected navigation state: %+v", got)
	}
	if got.PreviousURL != "/tools?kind=mcp&page=2&q=stripe" {
		t.Fatalf("previous URL=%q", got.PreviousURL)
	}
}

func TestPageSliceUsesClampedPage(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	got := pageSlice(items, pager{Page: 3}, 2)
	if len(got) != 1 || got[0] != 5 {
		t.Fatalf("pageSlice=%v", got)
	}
}

func TestDisplayLabels(t *testing.T) {
	if got := statusLabel("reauthorization_required"); got != "Needs authorization" {
		t.Fatalf("statusLabel=%q", got)
	}
	if got := kindLabel("mcp_http"); got != "Remote MCP" {
		t.Fatalf("kindLabel=%q", got)
	}
	if got := runtimeLabel("openclaw"); got != "OpenClaw" {
		t.Fatalf("runtimeLabel=%q", got)
	}
	if got := shorten("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("shorten=%q", got)
	}
}

func TestFlashIsReturnedOnce(t *testing.T) {
	flashes := newFlashStore()
	id := flashes.put(flash{Kind: "ok", Message: "saved", Token: "tmx_secret"})
	first := flashes.take(id)
	if first == nil || first.Token != "tmx_secret" {
		t.Fatalf("first take = %+v", first)
	}
	if flashes.take(id) != nil {
		t.Fatal("flash was returned twice")
	}
	if flashes.take("") != nil {
		t.Fatal("empty id returned a flash")
	}
}

func TestSameOriginRejectsCrossSitePosts(t *testing.T) {
	handler := sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	cases := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{"no browser headers", "/agents", nil, http.StatusNoContent},
		{"matching origin", "/agents", map[string]string{"Origin": "http://toolmux.local:8080"}, http.StatusNoContent},
		{"matching referer", "/agents", map[string]string{"Referer": "http://toolmux.local:8080/agents"}, http.StatusNoContent},
		{"foreign origin", "/agents", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"foreign referer", "/agents", map[string]string{"Referer": "https://evil.example/page"}, http.StatusForbidden},
		{"fetch metadata", "/agents", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"mcp endpoint is exempt", "/mcp", map[string]string{"Origin": "https://client.example"}, http.StatusNoContent},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, tc.path, nil)
		request.Host = "toolmux.local:8080"
		for name, value := range tc.headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, recorder.Code, tc.want)
		}
	}
}

func TestReturnTargetOnlyAcceptsLocalPaths(t *testing.T) {
	if got := returnTarget("/connections?status=connected", "/x"); got != "/connections?status=connected" {
		t.Fatalf("got %q", got)
	}
	for _, value := range []string{"", "https://evil.example", "//evil.example/path", "javascript:alert(1)"} {
		if got := returnTarget(value, "/fallback"); got != "/fallback" {
			t.Fatalf("%q was accepted as %q", value, got)
		}
	}
}
