package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	_ "cursorforge/internal/protocodec/gen/aiserver/v1"
)

// AdapterInfo carries the bits of a configured BYOK adapter that the rewriter
// needs to forge synthetic responses.
type AdapterInfo struct {
	DisplayName     string
	Type            string
	ModelID         string
	BaseURL         string
	APIKey          string
	ReasoningEffort string
	ServiceTier     string
	MaxOutputTokens int
	ThinkingBudget  int
}

// StableID returns a 16-char hex identifier derived from the provider model
// ID alone. Cursor caches this id across many SQLite keys
// (aiSettings.modelConfig.<feature>.modelName, featureModelConfigs.<feature>
// .defaultModel, etc.); deriving from ModelID — not display name — keeps the
// id stable when the user only renames the adapter, so Cursor's cached
// references remain valid.
func (a AdapterInfo) StableID() string {
	h := sha256.Sum256([]byte("byok|" + a.ModelID))
	return hex.EncodeToString(h[:8])
}

// rewriter is a per-path response transformer. It receives the upstream body
// (already gunzipped by the caller if needed) plus the user's adapter list
// and returns a new body to send back to Cursor.
type rewriter func(body []byte, adapters []AdapterInfo) ([]byte, error)

var rewrites = map[string]rewriter{
	"/aiserver.v1.AiService/AvailableModels": rewriteAvailableModels,
}

// SyntheticPath returns the hand-rolled response bytes (raw protobuf, no
// Connect envelope) for paths the working app implements via local mocks
// rather than upstream forwarding. Returns nil if the path has no synthetic
// response. Callers must wrap in a Connect envelope themselves if the
// request used Connect framing.
func (g *Gateway) SyntheticPath(path string) []byte {
	if g.adapterProvider == nil {
		return nil
	}
	adapters := g.adapterProvider()
	if len(adapters) == 0 {
		return nil
	}
	switch path {
	case "/aiserver.v1.AiService/GetDefaultModelNudgeData":
		return buildGetDefaultModelNudgeDataResponse(adapters)
	case "/aiserver.v1.AiService/AvailableModels":
		return buildAvailableModelsResponse(adapters)
	}
	return nil
}

// MaybeRewriteResponse swaps the body of any path we know how to forge. It
// transparently handles 5-byte Connect framing and HTTP-level gzip.
func (g *Gateway) MaybeRewriteResponse(req *http.Request, resp *http.Response) bool {
	if req == nil || resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	fn, ok := rewrites[req.URL.Path]
	if !ok || g.adapterProvider == nil {
		return false
	}
	adapters := g.adapterProvider()
	if len(adapters) == 0 {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return false
	}

	newBody, _, err := tryRewrite(body, adapters, fn)
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}

	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	resp.Header.Del("Content-Encoding")
	return true
}

// tryRewrite handles the wire-format dance: it tries the body as a raw
// protobuf payload first, then as a 5-byte Connect envelope (with optional
// per-frame gzip), and re-emits the result in the same shape.
func tryRewrite(body []byte, adapters []AdapterInfo, fn rewriter) (newBody []byte, framed bool, err error) {
	// Raw protobuf path.
	if out, rerr := fn(body, adapters); rerr == nil {
		return out, false, nil
	}
	// Connect envelope path.
	if len(body) >= 5 {
		flags := body[0]
		length := int(binary.BigEndian.Uint32(body[1:5]))
		if length >= 0 && len(body) == 5+length {
			payload := body[5:]
			if flags&0x01 != 0 {
				if up, gerr := gunzipBytes(payload); gerr == nil {
					payload = up
				}
			}
			out, rerr := fn(payload, adapters)
			if rerr != nil {
				return nil, true, rerr
			}
			env := make([]byte, 5+len(out))
			env[0] = 0
			binary.BigEndian.PutUint32(env[1:5], uint32(len(out)))
			copy(env[5:], out)
			return env, true, nil
		}
	}
	return nil, false, errors.New("body did not parse in raw or framed protobuf form")
}

// ---------------- Per-endpoint rewriters ----------------

func rewriteAvailableModels(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Build the response from scratch using the byte template that matches
	// the working app's wire format exactly. The proto types we ship are
	// missing several fields the picker relies on, so going through
	// proto.Marshal would lose them.
	_ = body
	return buildAvailableModelsResponse(adapters), nil
}

func rewriteGetDefaultModel(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Append-mode: don't override Cursor's default. Picker uses whatever
	// Cursor said and BYOK simply appears as an extra picker entry.
	return body, nil
}

func rewriteGetDefaultModelNudge(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Append-mode: leave the nudge data as Cursor sent it.
	return body, nil
}

func rewriteCppAvailableModels(body []byte, adapters []AdapterInfo) ([]byte, error) {
	// Append-mode: leave tab/cmd-K models alone.
	return body, nil
}

// (injectBYOKModels was deleted — buildAvailableModelsResponse in
// models_template.go now produces the response directly with byte-exact
// fidelity to the working app's wire format.)

func gunzipBytes(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// AvailableModelsPath kept for backwards compatibility with anything that
// imported the constant. Use the rewrites map for new code.
const AvailableModelsPath = "/aiserver.v1.AiService/AvailableModels"

var _ = fmt.Sprintf
