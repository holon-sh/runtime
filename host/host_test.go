package host

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
)

func TestAdmission(t *testing.T) {
	const (
		api      = "spiffe://cluster.local/ns/holon-system/sa/holon-api"
		reporter = "spiffe://cluster.local/ns/session/sa/reporter"
		job      = "spiffe://cluster.local/ns/session/sa/install-accounts"
	)
	valid := `{"exp":4102444800,"aud":["https://holon.example"],"principal":{"iss":"issuer","sub":"user","client_id":"client","scope":"read write"}}`
	anonymous := `{"exp":4102444800,"aud":["https://holon.example"]}`
	hook := `{"exp":4102444800,"aud":["https://holon.example"],"hook":{"app":"accounts","job":"install-accounts"}}`
	hookForPerson := `{"exp":4102444800,"aud":["https://holon.example"],"hook":{"app":"accounts","job":"install-accounts"},` +
		`"principal":{"iss":"issuer","sub":"user","client_id":"client","scope":"read write"}}`
	// A mistyped standard claim reads as absent; other claims pass through.
	profile := `{"address":{"country":1,"locality":"Tokyo"},"custom":{"values":[1,null]},"email_verified":"true","name":"Ada","updated_at":1}`
	admitted := func(fields, extra string) string { return `{"token":"TOKEN",` + fields + `,"extra":{` + extra + `}}` }
	person := `"clientId":"client","scopes":["read","write"],"expiresAt":4102444800,"resource":"https://holon.example"`
	nobody := `"clientId":"","scopes":[],"expiresAt":4102444800,"resource":"https://holon.example"`
	user := `"iss":"issuer","sub":"user"`
	reporting, installing := `"caller":{"app":"reporter","session":"session"}`, `"caller":{"app":"accounts","session":"session"}`
	for _, tc := range []struct {
		name, caller, claims, duplicate, identity string
	}{
		{"signed-in person", api, valid, "", admitted(person, user)},
		{"signed-out person", api, anonymous, "", "null"},
		{"platform discovery", api, "", "", "null"},
		{"app for a signed-in person", reporter, valid, "", admitted(person, reporting+","+user)},
		{"app for a signed-out person", reporter, anonymous, "", admitted(nobody, reporting)},
		{"app outside a request", reporter, "", "", `{"token":"","clientId":"","scopes":[],"expiresAt":0,"resource":"","extra":{` + reporting + `}}`},
		{"lifecycle hook job", job, hook, "", admitted(nobody, installing)},
		{"lifecycle hook job for a person", job, hookForPerson, "", admitted(person, installing+","+user)},
		{"app called by a hook job", reporter, hook, "", admitted(nobody, reporting)},
		{"expired", api, strings.Replace(valid, "4102444800", "1", 1), "", ""},
		{"multiple audiences", api, `{"exp":4102444800,"aud":["https://one.example","https://two.example"]}`, "", ""},
		{"relative audience", api, `{"exp":4102444800,"aud":["/relative"]}`, "", ""},
		{"invalid principal", api, strings.Replace(valid, `"client_id":"client"`, `"client_id":null`, 1), "", ""},
		{"invalid scopes", api, strings.Replace(valid, `"scope":"read write"`, `"scope":[]`, 1), "", ""},
		{"identity claims", api, strings.Replace(valid, `}}`, `,"claims":`+profile+`}}`, 1), "",
			admitted(person, `"claims":`+strings.NewReplacer(`"country":1,`, "", `"email_verified":"true",`, "").Replace(profile)+","+user)},
		{"non-object actor", api, strings.Replace(valid, `}}`, `,"act":[{"sub":"agent"}]}}`, 1), "", ""},
		{"caller outside the mesh", "https://edge", "", "", ""},
		{"two callers", api, "", runtimecontract.CallerHeader, ""},
		{"two contexts", api, valid, runtimecontract.ContextHeader, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(protocol.RuntimeConfigEnv, `{"version":2,"peers":{},"callers":["`+api+`"]}`)
			h, err := newHost()
			if err != nil {
				t.Fatal(err)
			}
			var identity string
			h.application = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				context := httptest.NewRequestWithContext(r.Context(), http.MethodGet, "/context", nil)
				context.SetPathValue("scope", r.Header.Get(runtimecontract.ScopeHeader))
				response := httptest.NewRecorder()
				h.context(response, context)
				identity = strings.TrimSpace(response.Body.String())
				w.WriteHeader(http.StatusNoContent)
			})
			h.frontend = h.application
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
			r.Header.Set(runtimecontract.CallerHeader, tc.caller)
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.claims)) + ".signature"
			if tc.claims != "" {
				r.Header.Set(runtimecontract.ContextHeader, token)
			}
			if tc.duplicate != "" {
				r.Header.Add(tc.duplicate, r.Header.Get(tc.duplicate))
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			want, cache := http.StatusNoContent, ""
			if tc.identity == "" {
				want, cache = http.StatusForbidden, "no-store"
			}
			if response.Code != want || response.Header().Get("Cache-Control") != cache {
				t.Fatalf("admission: %d %v", response.Code, response.Header())
			}
			if identity != strings.Replace(tc.identity, "TOKEN", token, 1) {
				t.Fatalf("identity: %s", identity)
			}
		})
	}
}

func TestPeerCalls(t *testing.T) {
	const token = "header.claims.signature"
	for _, tc := range []struct {
		name, scope, header, context string
		status                       int
	}{
		{"inside a request", "request", "", token, http.StatusOK},
		{"inside a workload call", "workload", "", "", http.StatusOK},
		{"outside a request", "", "", "", http.StatusOK},
		{"after the request ended", "ended", "", "", http.StatusOK},
		{"application credentials", "request", "Authorization", "", http.StatusForbidden},
		{"application context", "", runtimecontract.ContextHeader, "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received http.Header
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received = r.Header.Clone()
			}))
			t.Cleanup(peer.Close)
			t.Setenv(protocol.RuntimeConfigEnv, `{"version":2,"peers":{"crm":"`+peer.URL+`/mcp"}}`)
			h, err := newHost()
			if err != nil {
				t.Fatal(err)
			}
			ended, cancel := context.WithCancel(t.Context())
			cancel()
			trace := http.Header{"Traceparent": {"00-trace-span-01"}}
			h.scopes = map[string]*scope{
				"request":  {ctx: t.Context(), token: token, forward: http.Header{"Traceparent": trace["Traceparent"], runtimecontract.ContextHeader: {token}}},
				"workload": {ctx: t.Context(), forward: trace},
				"ended":    {ctx: ended, token: token, forward: trace},
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/peer/crm", nil)
			r.SetPathValue("peer", "crm")
			if tc.scope != "" {
				r.Header.Set(runtimecontract.ScopeHeader, tc.scope)
			}
			if tc.header != "" {
				r.Header.Set(tc.header, "Bearer app-token")
			}
			response := httptest.NewRecorder()
			h.peer(response, r)
			if response.Code != tc.status {
				t.Fatalf("status: %d %s", response.Code, response.Body)
			}
			if tc.status != http.StatusOK {
				if received != nil || response.Header().Get("X-Holon-Error") != "true" {
					t.Fatalf("refusal: forwarded %v, headers %v", received, response.Header())
				}
				return
			}
			traced := tc.scope != "" && tc.scope != "ended"
			if received.Get(runtimecontract.ContextHeader) != tc.context || received.Get(runtimecontract.ScopeHeader) != "" ||
				(received.Get("Traceparent") == trace.Get("Traceparent")) != traced {
				t.Fatalf("forwarded headers: %v", received)
			}
		})
	}
}

func TestPeerCallLifetime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound bool
	}{{"inbound request", true}, {"application request", false}} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			}))
			t.Cleanup(peer.Close)
			t.Setenv(protocol.RuntimeConfigEnv, `{"version":2,"peers":{"crm":"`+peer.URL+`/mcp"}}`)
			h, err := newHost()
			if err != nil {
				t.Fatal(err)
			}
			inbound, endInbound := context.WithCancel(t.Context())
			defer endInbound()
			h.scopes["request"] = &scope{ctx: inbound, token: "header.claims.signature"}
			application, endApplication := context.WithCancel(t.Context())
			defer endApplication()
			r := httptest.NewRequestWithContext(application, http.MethodPost, "/peer/crm", nil)
			r.SetPathValue("peer", "crm")
			r.Header.Set(runtimecontract.ScopeHeader, "request")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.peer(response, r)
			}()
			<-started
			if tc.inbound {
				endInbound()
			} else {
				endApplication()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("peer call outlived the request")
			}
			if response.Code != http.StatusBadGateway || response.Header().Get("X-Holon-Error") != "true" {
				t.Fatalf("cancelled call: %d %v", response.Code, response.Header())
			}
		})
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"peers":{}}`, `{"version":2}`, `{"version":2,"peers":{},"unknown":true}`,
		`{"version":2,"peers":{"peer":"https://peer/mcp"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(protocol.RuntimeConfigEnv, raw)
			if _, err := newHost(); err == nil {
				t.Fatal("configuration accepted")
			}
		})
	}
}
