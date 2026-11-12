package host

import (
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
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

// meshCaller is the single spiffe identity the waypoint asserts for a request, or "" without one.
func meshCaller(r *http.Request) string {
	caller := r.Header.Get(runtimecontract.CallerHeader)
	if len(r.Header.Values(runtimecontract.CallerHeader)) != 1 || !strings.HasPrefix(caller, "spiffe://") {
		return ""
	}
	return caller
}

func admit(r *http.Request, mesh string, platform bool) (*identity, bool) {
	contexts := r.Header.Values(runtimecontract.ContextHeader)
	if mesh == "" || len(contexts) > 1 {
		return nil, false
	}
	var claims struct {
		Expires   float64                  `json:"exp"`
		Audience  []string                 `json:"aud"`
		Principal map[string]any           `json:"principal"`
		Hook      *protocol.HookInvocation `json:"hook"`
	}
	admitted := &identity{Scopes: []string{}, Extra: map[string]any{}}
	// The platform's discovery and an app's call made outside a request carry no context.
	if len(contexts) == 1 {
		token := contexts[0]
		if len(token) > 16384 || runtimecontract.DecodeContextClaims(token, &claims) != nil ||
			claims.Expires <= float64(time.Now().UnixNano())/1e9 || len(claims.Audience) != 1 {
			return nil, false
		}
		if resource, err := url.Parse(claims.Audience[0]); err != nil || !resource.IsAbs() {
			return nil, false
		}
		admitted.Token, admitted.ExpiresAt, admitted.Resource = token, claims.Expires, claims.Audience[0]
	}
	// Besides the platform's own callers, only the apps of this app's session reach it, each as the service account
	// named after it; a lifecycle hook's job calls as its app.
	namespaced, inCluster := strings.CutPrefix(mesh, "spiffe://cluster.local/ns/")
	session, account, named := strings.Cut(namespaced, "/sa/")
	fromApp := inCluster && named && !platform
	if claims.Principal == nil && !fromApp {
		return nil, true
	}
	if fromApp {
		if claims.Hook != nil && account == claims.Hook.Job {
			account = claims.Hook.App
		}
		admitted.Extra["caller"] = map[string]string{"app": account, "session": session}
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
		if !standardTypes(profile, openIDClaims) {
			return nil, false
		}
		admitted.Extra["claims"] = profile
	}
	if actor := claims.Principal["act"]; actor != nil {
		if !typed[map[string]any](actor) {
			return nil, false
		}
		admitted.Extra["act"] = actor
	}
	admitted.ClientID = claims.Principal["client_id"].(string)
	return admitted, true
}

func typed[T any](value any) bool { _, ok := value.(T); return ok }

var isString = typed[string]

// The standard claims application SDKs type (OpenID Connect Core 1.0 §5.1); other claims pass through as JSON.
var openIDClaims = map[string]func(any) bool{
	"name": isString, "given_name": isString, "family_name": isString, "middle_name": isString,
	"nickname": isString, "preferred_username": isString, "profile": isString, "picture": isString,
	"website": isString, "email": isString, "email_verified": typed[bool], "gender": isString,
	"birthdate": isString, "zoneinfo": isString, "locale": isString, "phone_number": isString,
	"phone_number_verified": typed[bool], "updated_at": typed[float64],
	"address": func(value any) bool { return standardTypes(value, addressClaims) },
}

var addressClaims = map[string]func(any) bool{
	"formatted": isString, "street_address": isString, "locality": isString,
	"region": isString, "postal_code": isString, "country": isString,
}

// standardTypes reports whether value is a JSON object, after dropping its standard claims whose values lack the
// standard type: a provider's quirk then reads as an absent claim, as it does on the platform, not a refused request.
func standardTypes(value any, standard map[string]func(any) bool) bool {
	object, ok := value.(map[string]any)
	maps.DeleteFunc(object, func(name string, claim any) bool { valid, known := standard[name]; return known && !valid(claim) })
	return ok
}

func stripHolon(headers http.Header) {
	maps.DeleteFunc(headers, func(name string, _ []string) bool { return strings.HasPrefix(strings.ToLower(name), "x-holon-") })
}
