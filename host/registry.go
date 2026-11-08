package host

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var challengeParameter = regexp.MustCompile(`\b(realm|service|scope)="([^"]*)"`)

// registryLogin is an entry of a Docker config's auths.
type registryLogin struct {
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}

// loginFor reads the config on every fetch, so that a rotated pull secret applies to the next one. Without a usable
// entry the pull is anonymous and the registry's refusal is the error.
func loginFor(registry string) registryLogin {
	var config struct {
		Auths map[string]registryLogin `json:"auths"`
	}
	data, _ := os.ReadFile(filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"))
	_ = json.Unmarshal(data, &config)
	return cmp.Or(config.Auths[registry], config.Auths["https://"+registry])
}

// getBlob asks anonymously and, when challenged, again with a token from the endpoint the challenge names.
func (h *host) getBlob(ctx context.Context, blob *url.URL) (*http.Response, error) {
	client := &http.Client{Transport: h.transport}
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, blob.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(get)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	_ = response.Body.Close()
	token, err := exchangeToken(ctx, client, response.Header.Get("WWW-Authenticate"), loginFor(blob.Host))
	if err != nil {
		return nil, err
	}
	get.Header.Set("Authorization", "Bearer "+token)
	return client.Do(get)
}

// exchangeToken presents the login to the challenge's token endpoint: a password over GET, an identity token as an
// OAuth refresh grant.
func exchangeToken(ctx context.Context, client *http.Client, challenge string, login registryLogin) (string, error) {
	form := url.Values{}
	for _, parameter := range challengeParameter.FindAllStringSubmatch(challenge, -1) {
		form.Set(parameter[1], parameter[2])
	}
	realm := form.Get("realm")
	form.Del("realm")
	method, target, body := http.MethodGet, realm+"?"+form.Encode(), ""
	if login.IdentityToken != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", login.IdentityToken)
		form.Set("client_id", "holon-runtime")
		method, target, body = http.MethodPost, realm, form.Encode()
	}
	exchange, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	exchange.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if login.Auth != "" && login.IdentityToken == "" {
		exchange.Header.Set("Authorization", "Basic "+login.Auth)
	}
	response, err := client.Do(exchange)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint answered %d", response.StatusCode)
	}
	var grant struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	err = json.NewDecoder(response.Body).Decode(&grant)
	return cmp.Or(grant.Token, grant.AccessToken), err
}
