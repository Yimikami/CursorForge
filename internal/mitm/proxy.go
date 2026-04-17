package mitm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cursorforge/internal/agent"
	"cursorforge/internal/certs"
	"cursorforge/internal/relay"

	"github.com/elazarl/goproxy"
)

func gzipReader(b []byte) (io.ReadCloser, error) {
	return gzip.NewReader(bytes.NewReader(b))
}

type Server struct {
	addr string
	srv  *http.Server
	ln   net.Listener
	gw   *relay.Gateway
}

// agentResolver lets the agent package fetch the user's first BYOK adapter
// at request time. The bridge package wires this in so MITM doesn't need to
// know about UserConfig parsing — we just ask "give me a usable provider".
type AgentResolver = agent.AdapterResolver

func New(addr string, ca *certs.CA, gw *relay.Gateway, resolver AgentResolver) (*Server, error) {
	tlsCert, err := tls.X509KeyPair(ca.CertPEM(), ca.KeyPEM())
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, err
	}
	tlsCert.Leaf = leaf

	goproxy.GoproxyCa = tlsCert
	tlsCfg := goproxy.TLSConfigFromCA(&tlsCert)
	goproxy.OkConnect = &goproxy.ConnectAction{Action: goproxy.ConnectAccept, TLSConfig: tlsCfg}
	goproxy.MitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectMitm, TLSConfig: tlsCfg}
	goproxy.HTTPMitmConnect = &goproxy.ConnectAction{Action: goproxy.ConnectHTTPMitm, TLSConfig: tlsCfg}
	goproxy.RejectConnect = &goproxy.ConnectAction{Action: goproxy.ConnectReject, TLSConfig: tlsCfg}

	p := goproxy.NewProxyHttpServer()
	p.OnRequest().HandleConnect(goproxy.AlwaysMitm)

	// Mimic the working app's "everything secondary is 404" strategy. Cursor
	// pings dozens of auxiliary RPCs (auth profile, plan info, plugins,
	// statsig flags, etc.) and any one of them returning real upstream data
	// can flip the chat picker into "BYOK not allowed for your account" and
	// hide our injected models. The working app captures show only three
	// paths return real bodies; everything else gets a 404. Replicate that
	// exactly by allowlisting just the BYOK-critical paths and rejecting
	// the rest of api2.cursor.sh + auth host.
	// Paths we let through to the upstream so Cursor's real chat plumbing
	// runs (agent in/out streams). Everything else on api2/auth hosts gets
	// a 404 — matching the working app's captured behaviour byte-for-byte.
	allowedAPI2Paths := map[string]struct{}{
		"/aiserver.v1.BidiService/BidiAppend": {}, // agent input stream
		"/agent.v1.AgentService/RunSSE":       {}, // agent output SSE
		"/aiserver.v1.AiService/WriteGitCommitMessage": {},
		"/aiserver.v1.AiService/StreamBugBotAgenticSSE": {},
		"/aiserver.v1.BackgroundComposerService/AddAsyncFollowupBackgroundComposer": {},
		"/aiserver.v1.BackgroundComposerService/GetBackgroundComposerStatus": {},
		"/aiserver.v1.BackgroundComposerService/AttachBackgroundComposer": {},
		"/aiserver.v1.BackgroundComposerService/StreamInteractionUpdatesSSE": {},
	}
	authHosts := map[string]struct{}{
		"prod.authentication.cursor.sh":     {},
		"prod.authentication.cursor.sh:443": {},
		"authentication.cursor.sh":          {},
		"authentication.cursor.sh:443":      {},
	}
	api2Hosts := map[string]struct{}{
		"api2.cursor.sh":     {},
		"api2.cursor.sh:443": {},
	}
	// Synthetic raw-protobuf responses cloned from the working app captures.
	// Each entry is a hand-rolled body the gateway can produce on demand
	// (so it can be parameterised by the user's BYOK adapter list). We send
	// these with Content-Type: application/proto, no Connect envelope —
	// mirrors the captured working-app response shape exactly.
	syntheticAPI2Paths := map[string]struct{}{
		"/aiserver.v1.AiService/AvailableModels":          {},
		"/aiserver.v1.AiService/GetDefaultModelNudgeData": {},
	}
	mockProto := func(req *http.Request, body []byte) *http.Response {
		return &http.Response{
			Status:        "200 OK",
			StatusCode:    http.StatusOK,
			Proto:         req.Proto,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Header: http.Header{
				"Content-Type": {"application/proto"},
			},
			Request: req,
		}
	}
	mock404 := func(req *http.Request) *http.Response {
		body := "404 page not found\n"
		return &http.Response{
			Status:        "404 Not Found",
			StatusCode:    http.StatusNotFound,
			Proto:         req.Proto,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Header: http.Header{
				"Content-Type":           {"text/plain; charset=utf-8"},
				"X-Content-Type-Options": {"nosniff"},
			},
			Request: req,
		}
	}
	p.OnRequest().DoFunc(func(req *http.Request, _ *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		host := req.URL.Host
		path := req.URL.Path
		_, isAuthHost := authHosts[host]
		_, isAPI2Host := api2Hosts[host]
		if isAPI2Host {
			if _, ok := syntheticAPI2Paths[path]; ok && gw != nil {
				if body := gw.SyntheticPath(path); body != nil {
					return req, mockProto(req, body)
				}
			}
			if path == "/aiserver.v1.BidiService/BidiAppend" {
				return req, handleBidiAppend(req)
			}
			if path == "/agent.v1.AgentService/RunSSE" {
				return req, handleRunSSE(req, resolver)
			}
			if path == "/aiserver.v1.AiService/WriteGitCommitMessage" {
				return req, handleWriteGitCommitMessage(req, resolver)
			}
			if path == "/aiserver.v1.AiService/StreamBugBotAgenticSSE" {
				return req, handleBugBotRunSSE(req, resolver)
			}
			if path == "/aiserver.v1.BackgroundComposerService/AddAsyncFollowupBackgroundComposer" {
				return req, handleBackgroundComposerAddFollowup(req)
			}
			if path == "/aiserver.v1.BackgroundComposerService/GetBackgroundComposerStatus" {
				return req, handleBackgroundComposerStatus(req)
			}
			if path == "/aiserver.v1.BackgroundComposerService/AttachBackgroundComposer" {
				return req, handleBackgroundComposerAttach(req, resolver)
			}
			if path == "/aiserver.v1.BackgroundComposerService/StreamInteractionUpdatesSSE" {
				return req, handleBackgroundComposerInteractionUpdates(req)
			}
			if _, allowed := allowedAPI2Paths[path]; allowed {
				return req, nil
			}
			return req, mock404(req)
		}
		if isAuthHost {
			return req, mock404(req)
		}
		return req, nil
	})

	if gw != nil {
		p.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
			var req *http.Request
			if ctx != nil {
				req = ctx.Req
			}
			gw.MaybeRewriteResponse(req, resp)
			return resp
		})
	}

	return &Server{
		addr: addr,
		gw:   gw,
		srv:  &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second},
	}, nil
}

// handleBidiAppend reads the full request body (Cursor sends BidiAppend as a
// unary POST), hands it to the agent package, and packages the result as a
// goproxy-compatible http.Response.
func handleBidiAppend(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleBidiAppend(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{
		Status:        http.StatusText(res.Status),
		StatusCode:    res.Status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Header:        hdr,
		Request:       req,
	}
}

// handleRunSSE returns a streaming http.Response whose Body is an io.Pipe;
// a goroutine reads the request body once, calls into the agent package,
// and writes Connect SSE frames to the pipe writer. goproxy copies the
// pipe reader to the client connection so each frame reaches Cursor as
// soon as it's written (no buffering at our layer).
func handleRunSSE(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleRunSSE(req.Context(), body, req.Header.Get("Content-Type"), pw, resolver)
	}()
	hdr := http.Header{}
	for k, vs := range agent.RunSSEHeaders {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header:     hdr,
		Request:    req,
	}
}

func handleWriteGitCommitMessage(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleWriteGitCommitMessage(req.Context(), body, req.Header.Get("Content-Type"), resolver)
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{
		Status:        http.StatusText(res.Status),
		StatusCode:    res.Status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Header:        hdr,
		Request:       req,
	}
}

func handleBugBotRunSSE(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleBugBotRunSSE(req.Context(), body, req.Header.Get("Content-Type"), pw, resolver)
	}()
	hdr := http.Header{}
	for k, vs := range agent.BugBotSSEHeaders {
		for _, v := range vs {
			hdr.Add(k, v)
		}
	}
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header:     hdr,
		Request:    req,
	}
}

func handleBackgroundComposerAttach(req *http.Request, resolver agent.AdapterResolver) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleBackgroundComposerAttach(req.Context(), body, req.Header.Get("Content-Type"), pw, resolver)
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header: http.Header{
			"Content-Type":             {"text/event-stream"},
			"Cache-Control":            {"no-cache"},
			"Connect-Content-Encoding": {"gzip"},
			"Connect-Accept-Encoding":  {"gzip"},
		},
		Request: req,
	}
}

func handleBackgroundComposerInteractionUpdates(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		agent.HandleBackgroundComposerInteractionUpdates(req.Context(), body, req.Header.Get("Content-Type"), pw)
	}()
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Body:       pr,
		Header: http.Header{
			"Content-Type":             {"text/event-stream"},
			"Cache-Control":            {"no-cache"},
			"Connect-Content-Encoding": {"gzip"},
			"Connect-Accept-Encoding":  {"gzip"},
		},
		Request: req,
	}
}

func handleBackgroundComposerAddFollowup(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleAddAsyncBackgroundComposer(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{Status: http.StatusText(res.Status), StatusCode: res.Status, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: io.NopCloser(bytes.NewReader(res.Body)), ContentLength: int64(len(res.Body)), Header: hdr, Request: req}
}

func handleBackgroundComposerStatus(req *http.Request) *http.Response {
	body, err := readDecodedBody(req)
	if err != nil {
		return makeJSONResp(req, http.StatusBadRequest, `{"code":"invalid_argument","message":"read body: `+err.Error()+`"}`)
	}
	res := agent.HandleGetBackgroundComposerStatus(body, req.Header.Get("Content-Type"))
	hdr := http.Header{}
	if res.ContentType != "" {
		hdr.Set("Content-Type", res.ContentType)
	}
	return &http.Response{Status: http.StatusText(res.Status), StatusCode: res.Status, Proto: req.Proto, ProtoMajor: req.ProtoMajor, ProtoMinor: req.ProtoMinor, Body: io.NopCloser(bytes.NewReader(res.Body)), ContentLength: int64(len(res.Body)), Header: hdr, Request: req}
}

// readDecodedBody reads req.Body and transparently un-gzips when Cursor
// shipped the payload with HTTP-level gzip (Content-Encoding: gzip). The
// raw transport here is decoupled from any Connect envelope/compression
// the inner protobuf might also carry — that's still done by the agent
// package via decodeUnary.
func readDecodedBody(req *http.Request) ([]byte, error) {
	defer req.Body.Close()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if enc == "gzip" || enc == "x-gzip" {
		zr, gerr := gzipReader(raw)
		if gerr != nil {
			return nil, gerr
		}
		defer zr.Close()
		out, rerr := io.ReadAll(zr)
		if rerr != nil {
			return nil, rerr
		}
		return out, nil
	}
	return raw, nil
}

func makeJSONResp(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Proto:         req.Proto,
		ProtoMajor:    req.ProtoMajor,
		ProtoMinor:    req.ProtoMinor,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Header:        http.Header{"Content-Type": {"application/json"}},
		Request:       req,
	}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
