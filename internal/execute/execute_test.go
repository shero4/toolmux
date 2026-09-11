package execute

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sentinel-mcp/sentinel/internal/store"
)

func TestRenderStringRequiresInputs(t *testing.T) {
	got, err := renderString("/customers/${customer_id}", map[string]any{"customer_id": "cus/123"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/customers/cus%2F123" {
		t.Fatalf("got %q", got)
	}
	if _, err := renderString("${missing}", map[string]any{}, false); err == nil {
		t.Fatal("expected missing input error")
	}
}

func TestApplyNamedCredentialHeader(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	applyCredential(request, store.Connection{AuthMethod: "header", AuthName: "X-API-Key"}, store.Credential{BearerToken: "secret"})
	if got := request.Header.Get("X-API-Key"); got != "secret" {
		t.Fatalf("got %q", got)
	}
}

func TestSetEnvReplacesExistingValue(t *testing.T) {
	got := setEnv([]string{"PATH=/bin", "TOKEN=old"}, "TOKEN", "new")
	if len(got) != 2 || got[1] != "TOKEN=new" {
		t.Fatalf("unexpected environment: %#v", got)
	}
}

func TestRenderJSONPreservesExactValueType(t *testing.T) {
	template := map[string]any{"amount": "${amount}", "label": "invoice-${id}"}
	got, err := renderJSON(template, map[string]any{"amount": float64(42), "id": "7"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	if string(data) != `{"amount":42,"label":"invoice-7"}` {
		t.Fatalf("got %s", data)
	}
}

func TestLimitedBufferCapsMemory(t *testing.T) {
	buffer := &limitedBuffer{limit: 4}
	n, err := buffer.Write([]byte("123456"))
	if err != nil || n != 6 || buffer.String() != "1234" || !buffer.exceeded {
		t.Fatalf("unexpected buffer state: n=%d err=%v value=%q exceeded=%v", n, err, buffer.String(), buffer.exceeded)
	}
}
