package host

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/holon-sh/protocol/runtimecontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const contractPath = "/.holon/contract"

var clientExtensions = map[string]any{"io.modelcontextprotocol/files": map[string]any{"upload": map[string]any{}, "download": map[string]any{}}}

type listed struct {
	Pages []json.RawMessage `json:"pages"`
	Error string            `json:"error,omitempty"`
}

type listEventsParams struct {
	mcp.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

type listEventsResult struct {
	mcp.ResultBase
	Events     []json.RawMessage `json:"events"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

type scoped struct {
	base  http.RoundTripper
	scope string
}

func (s scoped) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.Header.Set(runtimecontract.ScopeHeader, s.scope)
	return s.base.RoundTrip(request)
}

func (h *host) contract(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	port, serving := h.port, h.application != nil
	h.mu.Unlock()
	if !serving {
		writeJSON(w, http.StatusServiceUnavailable, reloadFailure{Message: "application restarting"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), reloadTimeout)
	defer cancel()
	id := rand.Text()
	h.mu.Lock()
	h.scopes[id] = &scope{ctx: ctx, cancel: cancel, forward: http.Header{}}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.scopes, id)
		h.mu.Unlock()
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "holon-runtime", Version: "contract"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{Extensions: clientExtensions}})
	if err := mcp.AddSendingCustomMethod[*listEventsParams, *listEventsResult](client, "events/list"); err != nil {
		writeJSON(w, http.StatusInternalServerError, reloadFailure{Message: err.Error()})
		return
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://127.0.0.1:" + strconv.Itoa(int(port)) + "/mcp",
		HTTPClient: &http.Client{Transport: scoped{base: h.transport, scope: id}}, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, reloadFailure{Message: "application discovery failed: " + err.Error()})
		return
	}
	defer func() { _ = session.Close() }()
	lists := map[string]listed{}
	for _, method := range []string{"tools/list", "resources/list", "events/list"} {
		pages, err := listPages(ctx, session, method)
		lists[method] = listed{Pages: pages}
		if err != nil {
			lists[method] = listed{Error: err.Error()}
		}
	}
	initialized := session.InitializeResult()
	writeJSON(w, http.StatusOK, map[string]any{"serverInfo": initialized.ServerInfo, "capabilities": initialized.Capabilities, "lists": lists})
}

func listPages(ctx context.Context, session *mcp.ClientSession, method string) ([]json.RawMessage, error) {
	var pages []json.RawMessage
	for cursor := ""; ; {
		var result any
		var err error
		switch method {
		case "tools/list":
			result, err = session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		case "resources/list":
			result, err = session.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
		default:
			result, err = mcp.CallCustomMethod[*listEventsParams, *listEventsResult](ctx, session, method, &listEventsParams{Cursor: cursor})
		}
		if err != nil {
			return nil, err
		}
		page, _ := json.Marshal(result)
		var next struct{ NextCursor string }
		_ = json.Unmarshal(page, &next)
		if pages = append(pages, page); next.NextCursor == "" {
			return pages, nil
		}
		cursor = next.NextCursor
	}
}
