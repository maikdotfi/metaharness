// Package mcp calls Model Context Protocol servers and hands the rest of the
// tree plain agent.Tool values. It is the only package that knows the protocol
// exists, and the only one that imports the MCP SDK.
//
// A server is a source of tools and nothing more, so it is a value rather than a
// resource: mcp.HTTP and mcp.Stdio do no I/O, and the first call dials. There
// are two ways to turn one into tools. Server.Tools asks the server what it has
// and wraps each entry with the server's own schema — one line to wire any
// server. A package under mcp/, such as mcp/lightpanda, instead hand-writes the
// tools worth exposing as typed Go arguments over Server.Call. Both produce
// []agent.Tool, so which one an application uses changes one line and nothing
// downstream of it.
//
// Only tools/list and tools/call are used. Resources, prompts, sampling and
// elicitation are not, which is what keeps a server free of state on our side.
package mcp

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultTimeout bounds one request to a server. agent.Run sets no per-tool
// deadline of its own, so without this a hung server would hang a turn forever.
const DefaultTimeout = 60 * time.Second

// A Server is an MCP server we call. It holds no session state of its own and
// dials on first use, so constructing one cannot fail: an endpoint that is
// unreachable or a command that is not installed is reported by the first Tools
// or Call instead.
//
// One Server serves every agent session. A server that is itself stateful — a
// browser driver holding one page — will therefore see concurrent sessions
// interleave on it, which is the server's own business by design.
type Server struct {
	// dial opens a transport to the server. It is what HTTP and Stdio differ in,
	// and it cannot fail: reaching the far side is the handshake's problem.
	dial  func() sdk.Transport
	label string // command or endpoint, used to name the server until it names itself

	timeout    time.Duration
	bearer     string
	httpClient *http.Client
	observer   func(Event)

	mu      sync.Mutex
	session *sdk.ClientSession
	name    string // serverInfo.Name once the handshake has told us
}

// Option configures a Server. An option that does not apply to a transport is
// ignored by it: WithBearer and WithHTTPClient say nothing to a stdio server.
type Option func(*Server)

// WithBearer sends token as an Authorization header on every HTTP request.
func WithBearer(token string) Option { return func(s *Server) { s.bearer = token } }

// WithHTTPClient replaces the client used to reach an HTTP server, which is
// where a caller reaches for proxies, custom TLS, or an OAuth round tripper.
func WithHTTPClient(c *http.Client) Option { return func(s *Server) { s.httpClient = c } }

// WithTimeout bounds one request to the server — a tool call, or the tools
// listing — replacing DefaultTimeout.
func WithTimeout(d time.Duration) Option { return func(s *Server) { s.timeout = d } }

// HTTP returns a server reached over streamable HTTP at endpoint.
func HTTP(endpoint string, opts ...Option) *Server {
	s := newServer(endpoint, opts)
	s.dial = func() sdk.Transport {
		return &sdk.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: s.client(),
			// We only ever ask questions: no server-initiated message is read, so
			// the standalone SSE stream would be a connection held for nothing.
			DisableStandaloneSSE: true,
		}
	}
	return s
}

// Stdio returns a server run as a subprocess, spoken to over its stdin and
// stdout. The arguments are a slice rather than variadic so options can follow,
// matching agent.Command's Cmd/Args split.
//
// The subprocess starts on the first call and lives until Close, so a server
// that keeps state between calls keeps it.
func Stdio(command string, args []string, opts ...Option) *Server {
	s := newServer(command, opts)
	s.dial = func() sdk.Transport {
		// Deliberately not exec.CommandContext: the child belongs to the Server,
		// not to whichever call happened to start it.
		return &sdk.CommandTransport{Command: exec.Command(command, args...)}
	}
	return s
}

func newServer(label string, opts []Option) *Server {
	s := &Server{label: label, timeout: DefaultTimeout}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Close releases what is local: a stdio server's subprocess, and an HTTP
// server's connection. It never releases anything on the far side, and it does
// not make the Server unusable — a call after Close dials again. So `defer
// srv.Close()` is uniformly correct and never something to think about.
func (s *Server) Close() error {
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.mu.Unlock()

	if sess == nil {
		return nil
	}
	return sess.Close()
}

// connect returns the live session, dialing if there is none. The returned
// session may be closed underneath us at any time; call sites recover by
// dropping it and connecting again.
func (s *Server) connect(ctx context.Context) (*sdk.ClientSession, error) {
	sess, dialed, protocol, err := s.establish(ctx)
	switch {
	case err != nil:
		s.report(Event{Type: EventDialed, Server: s.serverName(), Err: err})
		return nil, err
	case dialed:
		s.report(Event{Type: EventDialed, Server: s.serverName(), Protocol: protocol})
	}
	return sess, nil
}

// establish is connect's critical section, split out so the observer never runs
// under the lock: it is free to ask the server anything, Close included.
func (s *Server) establish(ctx context.Context) (sess *sdk.ClientSession, dialed bool, protocol string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session != nil {
		return s.session, false, "", nil
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "metaharness", Version: "v0.1.0"}, nil)
	sess, err = client.Connect(ctx, s.dial(), nil)
	if err != nil {
		return nil, false, "", err
	}

	s.session = sess
	if init := sess.InitializeResult(); init != nil {
		protocol = init.ProtocolVersion
		if init.ServerInfo != nil && init.ServerInfo.Name != "" {
			s.name = init.ServerInfo.Name
		}
	}
	return sess, true, protocol, nil
}

// drop forgets sess if it is still the cached one, so the next call dials a
// fresh session, and closes it — which for stdio reaps a child that has usually
// died already.
func (s *Server) drop(sess *sdk.ClientSession) {
	s.mu.Lock()
	if s.session == sess {
		s.session = nil
	}
	s.mu.Unlock()
	_ = sess.Close()
}

// serverName is what the server called itself, falling back to how we reach it.
func (s *Server) serverName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.name != "" {
		return s.name
	}
	return s.label
}

func (s *Server) client() *http.Client {
	if s.bearer == "" {
		if s.httpClient != nil {
			return s.httpClient
		}
		return nil
	}
	base := s.httpClient
	if base == nil {
		base = http.DefaultClient
	}
	return &http.Client{
		Transport:     bearerTransport{token: s.bearer, base: base.Transport},
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
		Timeout:       base.Timeout,
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return base.RoundTrip(req)
}

// transportFailed reports whether err means the connection is gone rather than
// the server having answered. A JSON-RPC error is an answer; a cancelled
// context is the caller's doing. Everything else — a closed connection, a dead
// subprocess — is worth dialing again for.
func transportFailed(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var rpcErr *jsonrpc.Error
	return !errors.As(err, &rpcErr)
}
