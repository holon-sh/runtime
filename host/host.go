package host

import (
	"cmp"
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
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/protocol/runtimecontract"
)

type scope struct {
	ctx      context.Context
	cancel   context.CancelFunc
	token    string
	identity *identity
	forward  http.Header
}

type host struct {
	peers       map[string]*url.URL
	transport   *http.Transport
	image       string   // the application directory, which an overlay falls back to and which is never removed
	overlays    string   // the directory that holds each overlay's directory
	callers     []string // the mesh identities that may reload the application
	reloading   atomic.Bool
	reloads     chan *reloadJob
	mu          sync.Mutex
	application http.Handler // nil from a restart's start until its child is ready
	frontend    http.Handler
	starting    *process
	scopes      map[string]*scope
}

func newHost() (*host, error) {
	var peers map[string]*url.URL
	var settings runtimecontract.Config
	if raw, configured := os.LookupEnv(protocol.RuntimeConfigEnv); configured {
		if protocol.DecodeJSONNonNull([]byte(raw), &settings) != nil || settings.Version != 2 || settings.Peers == nil {
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
		peers: peers, transport: transport, scopes: map[string]*scope{},
		overlays: cmp.Or(os.Getenv(reloadDirEnv), defaultReloadDir),
		callers:  settings.Callers, reloads: make(chan *reloadJob),
	}, nil
}

func (h *host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A reload is admitted by its mesh caller, without platform context; any other caller is answered like any request.
	if caller := meshCaller(r); caller != "" && slices.Contains(h.callers, caller) && r.Method == http.MethodPost && r.URL.Path == reloadPath {
		h.reload(w, r)
		return
	}
	h.mu.Lock()
	application, frontend := h.application, h.frontend
	h.mu.Unlock()
	if application == nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Application restarting", http.StatusServiceUnavailable)
		return
	}
	r = r.Clone(r.Context())
	var identity *identity
	var token string
	if h.peers != nil {
		var accepted bool
		identity, accepted = admit(r)
		if !accepted {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Invalid platform request context", http.StatusForbidden)
			return
		}
		token = r.Header.Get(runtimecontract.ContextHeader)
		r.Header.Del("Authorization")
	}
	switch {
	case r.URL.Path == "/mcp":
	// Beneath the endpoint are the paths an application's extensions issue, such as the upload and download targets
	// of MCP files, which their tokens authorize. They share the frontend's origin, so what they serve never runs.
	case strings.HasPrefix(r.URL.Path, "/mcp/"):
		if path.Clean(r.URL.Path) != r.URL.Path || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	case slices.Contains([]string{http.MethodGet, http.MethodHead}, r.Method):
		frontend.ServeHTTP(w, r)
		return
	default:
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s := &scope{ctx: ctx, cancel: cancel, token: token, identity: identity, forward: http.Header{}}
	for _, name := range []string{"Traceparent", "Tracestate", runtimecontract.ContextHeader} {
		if value := r.Header.Get(name); value != "" {
			s.forward.Set(name, value)
		}
	}
	stripHolon(r.Header)
	id := rand.Text()
	h.mu.Lock()
	h.scopes[id] = s
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.scopes, id)
		h.mu.Unlock()
	}()
	r.Header.Set(runtimecontract.ScopeHeader, id)
	application.ServeHTTP(w, r.WithContext(ctx))
}

func (h *host) active(id string) *scope {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.scopes[id]; s != nil && s.ctx.Err() == nil {
		return s
	}
	return nil
}

func (h *host) context(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s := h.active(r.PathValue("scope"))
	if s == nil {
		http.Error(w, "Request context unavailable", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.identity)
}

func (h *host) peer(w http.ResponseWriter, r *http.Request) {
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
	s := h.active(r.Header.Get(runtimecontract.ScopeHeader))
	ctx := r.Context()
	if s != nil && s.token != "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(s.ctx)
		defer cancel()
		defer context.AfterFunc(r.Context(), cancel)()
	}
	proxy := h.proxy(func(p *httputil.ProxyRequest) {
		*p.Out.URL = *target
		p.Out.Host = target.Host
		p.Out.Header.Del(runtimecontract.ScopeHeader)
		if s != nil {
			maps.Copy(p.Out.Header, s.forward)
		}
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

func (h *host) connect(port uint16, frontend http.Handler) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(int(port))}
	application := h.proxy(func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.Host = p.In.Host
		for _, name := range []string{"X-Forwarded-Host", "X-Forwarded-Proto"} {
			p.Out.Header[name] = p.In.Header[name]
		}
	})
	h.mu.Lock()
	h.application, h.frontend = application, frontend
	h.mu.Unlock()
	// Connections to a previous child are dead, and a request that reuses one cannot always be retried.
	h.transport.CloseIdleConnections()
}

func (h *host) disconnect() {
	h.mu.Lock()
	h.application, h.frontend = nil, nil
	for _, s := range h.scopes {
		s.cancel()
	}
	h.mu.Unlock()
}
