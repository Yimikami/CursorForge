package protocodec

import (
	"encoding/binary"
	"errors"
	"io"
)

// Frame is a single Connect/gRPC framed message: 1 byte flags + 4 byte length + payload.
type Frame struct {
	Flags byte
	Data  []byte
}

// IsEnd reports whether this frame is the trailers/end-of-stream sentinel
// (Connect uses bit 1 for trailers; gRPC uses bit 7 for compression — different
// semantics, but a non-zero MSB is a strong signal we should stop trying to
// protobuf-decode).
func (f *Frame) IsEnd() bool { return f.Flags&0x02 != 0 }

// ReadFrame reads one length-prefixed frame from r. Returns io.EOF cleanly when
// the stream ends between frames.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [5]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		if n == 0 {
			return nil, io.EOF
		}
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[1:])
	if length > 32*1024*1024 { // 32 MiB cap — anything larger is almost certainly garbage.
		return nil, errors.New("protocodec: frame too large")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return &Frame{Flags: hdr[0], Data: data}, nil
}
