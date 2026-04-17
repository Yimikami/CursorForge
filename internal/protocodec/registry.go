package protocodec

import (
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// ParseConnectPath splits a Connect/gRPC URL path
// "/package.Service/Method" into its parts. Returns ok=false on malformed input.
func ParseConnectPath(path string) (svc, method string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, "/")
	idx := strings.Index(rest, "/")
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

// LookupResponseMessage returns a freshly-allocated response message for the
// given Connect path, or nil if the method is unknown to the registry.
func LookupResponseMessage(path string) proto.Message {
	svc, method, ok := ParseConnectPath(path)
	if !ok {
		return nil
	}
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	if err != nil {
		return nil
	}
	sd, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil
	}
	out := md.Output()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(out.FullName())
	if err != nil {
		return nil
	}
	return mt.New().Interface()
}
