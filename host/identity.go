package host

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/holon-sh/protocol/runtimecontract"
)

type identity struct {
	Token     string         `json:"token"`
	ClientID  string         `json:"clientId"`
	Scopes    []string       `json:"scopes"`
	ExpiresAt float64        `json:"expiresAt"`
	Resource  string         `json:"resource"`
	Extra     map[string]any `json:"extra"`
}

func admit(r *http.Request, callers []string) (*identity, bool) {
	token, caller := r.Header.Get(runtimecontract.ContextHeader), r.Header.Get(runtimecontract.CallerHeader)
	if len(r.Header.Values(runtimecontract.CallerHeader)) != 1 {
		return nil, false
	}
	discovery := runtimecontract.AppDiscoveryRequest(r.Method, r.URL.Path, r.Header.Get("Mcp-Method"))
	if token == "" && discovery && slices.Contains(callers, caller) {
		return nil, true
	}
	if len(r.Header.Values(runtimecontract.ContextHeader)) != 1 || len(token) > 16384 || !strings.HasPrefix(caller, "spiffe://") {
		return nil, false
	}
	var claims struct {
		Expires   float64        `json:"exp"`
		Audience  []string       `json:"aud"`
		Principal map[string]any `json:"principal"`
		Hook      map[string]any `json:"hook"`
	}
	if runtimecontract.DecodeContextClaims(token, &claims) != nil || claims.Expires <= float64(time.Now().UnixNano())/1e9 || len(claims.Audience) != 1 {
		return nil, false
	}
	resource, err := url.Parse(claims.Audience[0])
	if err != nil || !resource.IsAbs() {
		return nil, false
	}
	if claims.Principal == nil && claims.Hook == nil {
		return nil, true
	}
	admitted := &identity{
		Token: token, Scopes: []string{}, ExpiresAt: claims.Expires,
		Resource: claims.Audience[0], Extra: map[string]any{"caller": caller},
	}
	if claims.Hook != nil {
		admitted.Extra["hook"] = claims.Hook
	}
	if claims.Principal == nil {
		return admitted, true
	}
	if slices.ContainsFunc([]string{"iss", "sub", "client_id"}, func(name string) bool { value, _ := claims.Principal[name].(string); return value == "" }) {
		return nil, false
	}
	if value, present := claims.Principal["scope"]; present {
		scope, ok := value.(string)
		if !ok {
			return nil, false
		}
		admitted.Scopes = strings.FieldsFunc(scope, func(char rune) bool { return char == ' ' })
	}
	admitted.Extra["iss"], admitted.Extra["sub"] = claims.Principal["iss"], claims.Principal["sub"]
	if profile, present := claims.Principal["claims"]; present {
		if _, ok := profile.(map[string]any); !ok {
			return nil, false
		}
		admitted.Extra["claims"] = profile
	}
	if actor, present := claims.Principal["act"]; present {
		admitted.Extra["act"] = actor
	}
	admitted.ClientID = claims.Principal["client_id"].(string)
	return admitted, true
}

func stripHolon(headers http.Header) {
	maps.DeleteFunc(headers, func(name string, _ []string) bool { return strings.HasPrefix(strings.ToLower(name), "x-holon-") })
}
