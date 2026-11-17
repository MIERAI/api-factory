// Package protoload 用 buf build 读取 proto 描述符(prepare 和 accept 共用)。
// proto 是语言无关的契约,所以验收执行器读它也不破坏"与运行时语言无关"。
package protoload

import (
	"fmt"
	"os"
	"os/exec"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

func Load() (*protoregistry.Files, error) {
	if err := os.MkdirAll(".factory", 0o755); err != nil {
		return nil, err
	}
	// 每个进程用自己的输出文件: 审查并行跑时,共用一个文件会读到别人写了一半的内容(实测: "proto: not found")
	f, err := os.CreateTemp(".factory", "image-*.binpb")
	if err != nil {
		return nil, err
	}
	out := f.Name()
	f.Close()
	defer os.Remove(out)
	cmd := exec.Command("buf", "build", "-o", out)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("buf build: %w", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		return nil, err
	}
	return protodesc.NewFiles(&set)
}

func FindMethod(files *protoregistry.Files, pkg, service, method string) (protoreflect.MethodDescriptor, error) {
	d, err := files.FindDescriptorByName(protoreflect.FullName(pkg + "." + service))
	if err != nil {
		return nil, fmt.Errorf("service %s.%s: %w", pkg, service, err)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%s.%s is not a service", pkg, service)
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil, fmt.Errorf("method %s not found in %s", method, service)
	}
	return md, nil
}
