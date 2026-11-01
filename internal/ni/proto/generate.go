//go:build ignore

// This program makes nidaqmx.pb.go and session.pb.go from the two .proto files
// beside it. Run it with `go generate ./internal/ni/proto` from the repository
// root. It needs no protoc: the compiler is bufbuild/protocompile and the code
// generator is the one protoc-gen-go is built on, both already in go.mod.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	files := []string{"session.proto", "nidaqmx.proto"}
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{"."}})}
	res, err := c.Compile(context.Background(), files...)
	if err != nil {
		fatal(err)
	}
	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: files,
		Parameter:      proto.String("paths=source_relative"),
	}
	seen := map[string]bool{}
	var add func(fd protoreflect.FileDescriptor)
	add = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			add(imps.Get(i).FileDescriptor)
		}
		req.ProtoFile = append(req.ProtoFile, protodesc.ToFileDescriptorProto(fd))
	}
	for _, f := range res {
		add(f)
	}
	gen, err := protogen.Options{}.New(req)
	if err != nil {
		fatal(err)
	}
	for _, f := range gen.Files {
		if f.Generate {
			internal_gengo.GenerateFile(gen, f)
		}
	}
	out := gen.Response()
	if out.Error != nil {
		fatal(fmt.Errorf("%s", *out.Error))
	}
	for _, f := range out.File {
		body := notice + f.GetContent()
		if err := os.WriteFile(filepath.Base(f.GetName()), []byte(body), 0o644); err != nil {
			fatal(err)
		}
	}
}

const notice = `// Derived from ni/grpc-device generated/nidaqmx/nidaqmx.proto and ni/ni-apis
// ni/grpcdevice/v1/session.proto, at v2.20.0 (MIT), Copyright (c) 2022, National
// Instruments Corp. Modified: cut down to the messages Flow calls, then
// generated. Only the tests use this package; the binary does not carry it.

`

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
