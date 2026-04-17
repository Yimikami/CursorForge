package relay

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"strings"

	"cursorforge/internal/protocodec"

	"google.golang.org/protobuf/proto"
)

const connectFlagCompressed = 0x01
const connectFlagEndStream = 0x02

func isCursorRPC(path string) bool {
	return strings.HasPrefix(path, "/aiserver.v1.") ||
		strings.HasPrefix(path, "/agent.v1.") ||
		strings.HasPrefix(path, "/anyrun.v1.") ||
		strings.HasPrefix(path, "/internapi.v1.")
}

type Gateway struct {
	adapterProvider func() []AdapterInfo
}

// SetAdapterProvider installs a callback that returns the user's currently
// configured BYOK adapters. Called by ProxyService.
func (g *Gateway) SetAdapterProvider(fn func() []AdapterInfo) { g.adapterProvider = fn }

func NewGateway() *Gateway {
	return &Gateway{}
}

func decodeFramed(body []byte, respMsg proto.Message) (int, int, protocodec.Stats) {
	var s protocodec.Stats
	frames, decoded := 0, 0
	for off := 0; off+5 <= len(body); {
		flags := body[off]
		length := int(binary.BigEndian.Uint32(body[off+1 : off+5]))
		if length < 0 || off+5+length > len(body) {
			break
		}
		frames++
		payload := body[off+5 : off+5+length]
		off += 5 + length
		if flags&connectFlagEndStream != 0 {
			continue
		}
		if flags&connectFlagCompressed != 0 {
			if gp, gerr := gunzip(payload); gerr == nil {
				payload = gp
			}
		}
		proto.Reset(respMsg)
		if uerr := proto.Unmarshal(payload, respMsg); uerr != nil {
			continue
		}
		decoded++
		s.Add(protocodec.Extract(respMsg))
	}
	return frames, decoded, s
}

func isGzipMagic(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

