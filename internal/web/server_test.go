package web

import (
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
