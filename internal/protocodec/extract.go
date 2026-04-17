package protocodec

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Stats are the metrics we care about across an entire response stream.
type Stats struct {
	PromptTokens     int64
	CompletionTokens int64
	Model            string
}

// Add merges s2 into s.
func (s *Stats) Add(s2 Stats) {
	s.PromptTokens += s2.PromptTokens
	s.CompletionTokens += s2.CompletionTokens
	if s.Model == "" && s2.Model != "" {
		s.Model = s2.Model
	}
}

// Extract walks any proto message and pulls out any field whose name matches
// our well-known set. Fields are summed across the tree (so frame-level usage
// blocks add up over a stream).
func Extract(m proto.Message) Stats {
	var s Stats
	if m == nil {
		return s
	}
	walk(m.ProtoReflect(), &s)
	return s
}

func walk(m protoreflect.Message, s *Stats) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		switch name {
		case "model_name", "model_id", "actual_model_name", "server_model_name":
			if fd.Kind() == protoreflect.StringKind && s.Model == "" {
				s.Model = v.String()
			}
		case "num_prompt_tokens", "prompt_tokens", "input_tokens":
			if isInt(fd.Kind()) {
				s.PromptTokens += v.Int()
			}
		case "num_completion_tokens", "completion_tokens", "output_tokens":
			if isInt(fd.Kind()) {
				s.CompletionTokens += v.Int()
			}
		}
		switch {
		case fd.IsList():
			if fd.Kind() == protoreflect.MessageKind {
				lst := v.List()
				for i := 0; i < lst.Len(); i++ {
					walk(lst.Get(i).Message(), s)
				}
			}
		case fd.IsMap():
			// Skip — Cursor schema map values don't carry usage data.
		case fd.Kind() == protoreflect.MessageKind:
			walk(v.Message(), s)
		}
		return true
	})
}

func isInt(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return true
	}
	return false
}
