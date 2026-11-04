package host

import (
	"cmp"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
)

func TestAdmission(t *testing.T) {
	const api = "spiffe://cluster.local/ns/control/sa/api"
	const authz = "spiffe://cluster.local/ns/control/sa/authz"
	valid := `{"exp":4102444800,"aud":["https://holon.example"],"principal":{"iss":"issuer","sub":"user","client_id":"client"}}`
	for _, tc := range []struct {
		name, config, request, caller, claims string
		admitted                              bool
	}{
		{"valid context", "", "POST /mcp", "spiffe://edge", valid, true},
		{"expired", "", "POST /mcp", "spiffe://edge", strings.Replace(valid, "4102444800", "1", 1), false},
		{"missing expiry", "", "POST /mcp", "spiffe://edge", `{"aud":["https://holon.example"]}`, false},
		{"multiple audiences", "", "POST /mcp", "spiffe://edge", `{"exp":4102444800,"aud":["https://one.example","https://two.example"]}`, false},
		{"relative audience", "", "POST /mcp", "spiffe://edge", `{"exp":4102444800,"aud":["/relative"]}`, false},
		{"invalid principal", "", "POST /mcp", "spiffe://edge", strings.Replace(valid, `"client_id":"client"`, `"client_id":null`, 1), false},
		{"invalid scopes", "", "POST /mcp", "spiffe://edge", strings.Replace(valid, `"client_id":"client"`, `"client_id":"client","scope":[]`, 1), false},
		{"control plane discovery", "", "POST /mcp server/discover", api, "", true},
		{"control plane probe", "", "HEAD /", api, "", true},
		{"control plane tool call", "", "POST /mcp tools/call", api, "", false},
		{"control plane page", "", "GET /", api, "", false},
		{"other caller", "", "POST /mcp tools/list", "spiffe://cluster.local/ns/control/sa/other-service", "", false},
		{"second configured caller", "", "POST /mcp tools/list", authz, "", true},
		{"no configured callers", `{"version":2,"peers":{}}`, "POST /mcp tools/list", api, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(protocol.RuntimeConfigEnv, cmp.Or(tc.config, `{"version":2,"peers":{},"callers":["`+api+`","`+authz+`"]}`))
			h, err := newHost()
			if err != nil {
				t.Fatal(err)
			}
			h.application = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			h.frontend = h.application
			method, target, _ := strings.Cut(tc.request, " ")
			target, mcpMethod, _ := strings.Cut(target, " ")
			r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
			r.Header.Set("Mcp-Method", mcpMethod)
			r.Header.Set(runtimecontract.CallerHeader, tc.caller)
			if tc.claims != "" {
				r.Header.Set(runtimecontract.ContextHeader, "header."+base64.RawURLEncoding.EncodeToString([]byte(tc.claims))+".signature")
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			want, cache := http.StatusNoContent, ""
			if !tc.admitted {
				want, cache = http.StatusForbidden, "no-store"
			}
			if response.Code != want || response.Header().Get("Cache-Control") != cache {
				t.Fatalf("admission: %d %v", response.Code, response.Header())
			}
		})
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"peers":{}}`, `{"version":2}`, `{"version":2,"peers":{},"unknown":true}`,
		`{"version":2,"peers":{},"callers":["spiffe://edge","https://edge"]}`,
		`{"version":2,"peers":{"peer":"https://peer/mcp"}}`, `{"version":2,"peers":{"peer":"http://user@peer/mcp"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(protocol.RuntimeConfigEnv, raw)
			if _, err := newHost(); err == nil {
				t.Fatal("configuration accepted")
			}
		})
	}
}
