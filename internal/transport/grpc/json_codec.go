package grpc

import "encoding/json"

// jsonCodec is a temporary transport codec for Muninn's manually-authored pb
// structs, which do not implement google.golang.org/protobuf/proto.Message.
//
// Until the service types are regenerated as real protobuf message types,
// forcing a JSON codec keeps the gRPC transport operational for local clients
// that need streaming Activate/Subscribe semantics.
type jsonCodec struct{}

func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (jsonCodec) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
