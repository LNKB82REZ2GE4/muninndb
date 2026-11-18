package grpc

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// jsonCodec is the fork's JSON transport codec for local gRPC clients.
//
// History: it was introduced when the committed pb types were hand-written
// structs that did not implement proto.Message, so the default proto codec
// could not decode any RPC, and the server FORCED this codec for every call.
// Upstream #874 (fixing #873) regenerated the stubs with protoc-gen-go, so the
// standard proto codec works again and forcing JSON would now break every
// standard protoc-gen-go client (and upstream's TestGRPCWireRoundTrip).
//
// It is therefore no longer forced. It is REGISTERED under the content-subtype
// "json", so a client that asks for it — grpc-go's grpc.ForceCodec(jsonCodec)
// or grpc.CallContentSubtype("json"), i.e. content-type
// "application/grpc+json" — still gets JSON, and every other client gets
// protobuf. The generated message structs carry json tags, so encoding/json
// handles them; their unexported protoimpl fields are ignored.
//
// Residual: a client that sent JSON bodies under the plain "application/grpc"
// content-type relied on the old forced codec and must now set the json
// content-subtype.
type jsonCodec struct{}

func init() { encoding.RegisterCodec(jsonCodec{}) }

func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jsonCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
