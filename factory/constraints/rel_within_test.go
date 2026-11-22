package constraints

import (
	"errors"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// "A 到 B 不超过 N 天" 用于日期: 生成的 CEL 在 protovalidate 里真的能跑(日期是 "YYYY-MM-DD" 的字符串)。
// 边界: 正好 N 天合法、N + 1 天违反;格式不对时这条规则不报(由字段的格式规则报);只指定一个时不检查。
func TestWithinDatesUnderProtovalidate(t *testing.T) {
	_, c, err := CompileRelation("d1 到 d2 不超过 3 天", lookup)
	if err != nil {
		t.Fatal(err)
	}
	opts := &descriptorpb.MessageOptions{}
	proto.SetExtension(opts, validate.E_Message, &validate.MessageRules{Cel: []*validate.Rule{{Id: proto.String("within.d1.d2.3"), Message: proto.String(c.Message), Expression: proto.String(c.Expr)}}})
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	fd := &descriptorpb.FileDescriptorProto{
		Name: proto.String("within_test.proto"), Package: proto.String("withintest"), Syntax: proto.String("proto3"),
		Dependency: []string{"buf/validate/validate.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Req"), Options: opts,
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("d1"), Number: proto.Int32(1), Type: &str, JsonName: proto.String("d1")},
				{Name: proto.String("d2"), Number: proto.Int32(2), Type: &str, JsonName: proto.String("d2"), Proto3Optional: proto.Bool(true), OneofIndex: proto.Int32(0)},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_d2")}},
		}},
	}
	file, err := protodesc.NewFile(fd, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	md := file.Messages().ByName("Req")
	v, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		d1, d2 string // d2 "" = 不指定
		valid  bool
	}{
		{"2026-01-01", "2026-01-04", true},  // 正好 3 天
		{"2026-01-01", "2026-01-05", false}, // 4 天
		{"2026-02-27", "2026-03-02", true},  // 跨月(平年 2 月): 3 天
		{"2024-02-27", "2024-03-02", false}, // 闰年: 4 天
		{"2026-01-01", "", true},            // 只指定一个
		{"2026/01/01", "2026-01-09", true},  // 格式不对: 这条规则不报(字段的格式规则报)
		{"2026-01-05", "2026-01-01", true},  // 反过来(早于 是另一条规则)
	} {
		m := dynamicpb.NewMessage(md)
		m.Set(md.Fields().ByName("d1"), protoreflect.ValueOfString(x.d1))
		if x.d2 != "" {
			m.Set(md.Fields().ByName("d2"), protoreflect.ValueOfString(x.d2))
		}
		err := v.Validate(m)
		var verr *protovalidate.ValidationError
		switch {
		case x.valid && err != nil:
			t.Errorf("%s 到 %s: 应该合法,实际 %v", x.d1, x.d2, err)
		case !x.valid && !errors.As(err, &verr):
			t.Errorf("%s 到 %s: 应该违反规则,实际 %v", x.d1, x.d2, err)
		}
	}
}
