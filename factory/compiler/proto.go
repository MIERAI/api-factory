package compiler

// proto 描述符 → IR(字段、类型、presence、buf.validate 规则、rpc 注释)。

import (
	"strings"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"apifactory/factory/constraints"
	"apifactory/factory/ir"
)

func convMessage(md protoreflect.MessageDescriptor, depth int) ir.Message {
	m := ir.Message{Name: string(md.Name())}
	if opts, ok := md.Options().(*descriptorpb.MessageOptions); ok && opts != nil && proto.HasExtension(opts, validate.E_Message) {
		m.HasRules = true
	}
	for i := 0; i < md.Fields().Len(); i++ {
		m.Fields = append(m.Fields, convField(md.Fields().Get(i), depth))
	}
	return m
}

func convField(fd protoreflect.FieldDescriptor, depth int) ir.Field {
	f := ir.Field{
		Name:     string(fd.Name()),
		Optional: fd.HasOptionalKeyword(),
		Presence: fd.HasPresence(),
		List:     fd.IsList(),
		Rules:    convRules(fd),
	}
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		f.Kind = "int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		f.Kind = "int64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		f.Kind = "uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		f.Kind = "uint64"
	case protoreflect.StringKind:
		f.Kind = "string"
	case protoreflect.BoolKind:
		f.Kind = "bool"
	case protoreflect.DoubleKind:
		f.Kind = "double"
	case protoreflect.FloatKind:
		f.Kind = "float"
	case protoreflect.EnumKind:
		f.Kind = "enum"
		f.Enum = string(fd.Enum().FullName())
		for i := 0; i < fd.Enum().Values().Len(); i++ {
			v := fd.Enum().Values().Get(i)
			f.Values = append(f.Values, ir.EnumValue{Name: string(v.Name()), Number: int32(v.Number())})
		}
	case protoreflect.MessageKind:
		if fd.Message().FullName() == "google.protobuf.Timestamp" {
			f.Kind = "timestamp"
		} else {
			f.Kind = "message"
			if depth < 3 {
				sub := convMessage(fd.Message(), depth+1)
				f.Message = &sub
			}
		}
	default:
		f.Kind = strings.ToLower(fd.Kind().String())
	}
	return f
}

func ptr[T any](v T) *T { return &v }

func convRules(fd protoreflect.FieldDescriptor) *ir.Rules {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, validate.E_Field) {
		return nil
	}
	r, _ := proto.GetExtension(opts, validate.E_Field).(*validate.FieldRules)
	if r == nil {
		return nil
	}
	out := fieldRules(r, fd)
	out.Raw = constraints.Raw(r)
	return out
}

// fieldRules: buf.validate 的规则 → IR(其余的规则由 factory/constraints 从 Raw 读取)。
func fieldRules(r *validate.FieldRules, _ protoreflect.FieldDescriptor) *ir.Rules {
	out := &ir.Rules{Required: r.GetRequired()}
	if x := r.GetRepeated(); r.HasRepeated() && x.HasMinItems() {
		out.MinItems = ptr(x.GetMinItems())
	}
	return out
}

// summary: proto 里 rpc 的注释第一行。
func summary(md protoreflect.MethodDescriptor) string {
	loc := md.ParentFile().SourceLocations().ByDescriptor(md)
	for _, line := range strings.Split(loc.LeadingComments, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
