package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
)

type scope struct {
	ctx      context.Context
	token    string
	identity *identity
	trace    http.Header
}

type host struct {
	callers     []string
	peers       map[string]*url.URL
	transport   *http.Transport
	application http.Handler
	frontend    http.Handler
	mu          sync.Mutex
	scopes      map[string]*scope
}

func newHost() (*host, error) {
	var peers map[string]*url.URL
	var settings runtimecontract.Config
	if raw, configured := os.LookupEnv(protocol.RuntimeConfigEnv); configured {
		if protocol.DecodeJSONNonNull([]byte(raw), &settings) != nil || settings.Version != 2 || settings.Peers == nil ||
			slices.ContainsFunc(settings.Callers, func(caller string) bool { return !strings.HasPrefix(caller, "spiffe://") }) {
			return nil, errors.New("invalid platform identity configuration")
		}
		peers = make(map[string]*url.URL, len(settings.Peers))
		for name, endpoint := range settings.Peers {
			u, err := url.Parse(endpoint)
			if err != nil || u.Host == "" || *u != (url.URL{Scheme: "http", Host: u.Host, Path: "/mcp"}) || u.String() != endpoint {
				return nil, errors.New("invalid platform peer endpoint")
			}
			peers[name] = u
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &host{
		callers: settings.Callers, peers: peers, transport: transport,
		scopes: map[string]*scope{},
	}, nil
}

func (h *host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.Clone(r.Context())
	var identity *identity
	var token string
	if h.peers != nil {
		var accepted bool
		identity, accepted = admit(r, h.callers)
		if !accepted {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Invalid platform request context", http.StatusForbidden)
			return
		}
		token = r.Header.Get(runtimecontract.ContextHeader)
		r.Header.Del("Authorization")
	}
	if r.URL.Path != "/mcp" {
		if slices.Contains([]string{http.MethodGet, http.MethodHead}, r.Method) {
			h.frontend.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	stripHolon(r.Header)
	id := rand.Text()
	s := &scope{ctx: r.Context(), token: token, identity: identity, trace: http.Header{}}
	for _, name := range []string{"Traceparent", "Tracestate"} {
		if value := r.Header.Get(name); value != "" {
			s.trace.Set(name, value)
		}
	}
	h.mu.Lock()
	h.scopes[id] = s
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.scopes, id)
		h.mu.Unlock()
	}()
	r.Header.Set(runtimecontract.ScopeHeader, id)
	h.application.ServeHTTP(w, r)
}

func (h *host) context(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.mu.Lock()
	s := h.scopes[r.PathValue("scope")]
	h.mu.Unlock()
	if s == nil || s.ctx.Err() != nil {
		http.Error(w, "Request context unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.identity)
}

func (h *host) peer(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s := h.scopes[r.Header.Get(runtimecontract.ScopeHeader)]
	h.mu.Unlock()
	target, found := h.peers[r.PathValue("peer")]
	if !found || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	for name := range r.Header {
		if strings.EqualFold(name, "Authorization") ||
			strings.HasPrefix(strings.ToLower(name), "x-holon-") && name != runtimecontract.ScopeHeader {
			peerError(w, "Platform peer credentials cannot be supplied by applications", http.StatusForbidden)
			return
		}
	}
	if len(r.Header.Values(runtimecontract.ScopeHeader)) != 1 || s == nil || s.token == "" || s.ctx.Err() != nil {
		peerError(w, "Peer calls require an active incoming request", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	defer cancel()
	proxy := h.proxy(func(p *httputil.ProxyRequest) {
		*p.Out.URL = *target
		p.Out.Host = target.Host
		p.Out.Header.Del(runtimecontract.ScopeHeader)
		p.Out.Header.Set(runtimecontract.ContextHeader, s.token)
		maps.Copy(p.Out.Header, s.trace)
	})
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return errors.New("platform peers cannot redirect")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		peerError(w, "Peer unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

func peerError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("X-Holon-Error", "true")
	http.Error(w, message, status)
}

func (h *host) proxy(rewrite func(*httputil.ProxyRequest)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: rewrite, Transport: h.transport, FlushInterval: -1,
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Application unavailable", http.StatusBadGateway)
		},
	}
}

func (h *host) connect(port uint16) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(int(port))}
	h.application = h.proxy(func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.Host = p.In.Host
		for _, name := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
			p.Out.Header[name] = p.In.Header[name]
		}
	})
}
