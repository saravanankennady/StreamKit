// Package joinrequest reads the join_request parameter of the single peer connection protocol. It stands alone because both
// the signalling service and the routing in front of it need to read one, and routing should not depend on the whole SFU.
package joinrequest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"

	"elnova-streamkit.local/pkg/pb"
)

// maxBytes limits a join request, compressed or not. It is the most a request line and headers can carry.
const maxBytes = http.DefaultMaxHeaderBytes

// Errors from Decode. Clients see these as an HTTP 400.
var (
	ErrMissing  = errors.New("join_request is required")
	ErrEncoding = errors.New("cannot base64 decode wrapped join request")
	ErrWrapped  = errors.New("cannot unmarshal wrapped join request")
	ErrContents = errors.New("cannot unmarshal join request")
	ErrGzip     = errors.New("cannot read decompressed join request")
	ErrTooLarge = errors.New("join request is too large")
)

// Decode reads a base64 URL-encoded WrappedJoinRequest, whose contents may be gzipped. This is client-controlled input, and
// decompression has its limit enforced as it reads, or a small request could expand to any size.
func Decode(encoded string) (*pb.JoinRequest, error) {
	if encoded == "" {
		return nil, ErrMissing
	}
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrEncoding
	}
	wrapped := &pb.WrappedJoinRequest{}
	if err := proto.Unmarshal(raw, wrapped); err != nil {
		return nil, ErrWrapped
	}
	contents := wrapped.GetJoinRequest()
	if wrapped.GetCompression() == pb.WrappedJoinRequest_GZIP {
		reader, err := gzip.NewReader(bytes.NewReader(contents))
		if err != nil {
			return nil, ErrGzip
		}
		contents, err = io.ReadAll(io.LimitReader(reader, maxBytes+1))
		if err != nil {
			return nil, ErrGzip
		}
	}
	if len(contents) > maxBytes {
		return nil, ErrTooLarge
	}
	join := &pb.JoinRequest{}
	if err := proto.Unmarshal(contents, join); err != nil {
		return nil, ErrContents
	}
	return join, nil
}
