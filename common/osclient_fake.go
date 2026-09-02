package common

import (
	"context"

	"google.golang.org/protobuf/proto"
)

// FakeOsClient is a configurable OsClient test double: set only the function
// fields your test needs; unset fields succeed with zero values.
//
// It is exported (not a _test.go file) so that agent/worker/gateway tests in
// other packages can reuse it (osclient.md §6).
type FakeOsClient struct {
	RunCommandFn      func(ctx context.Context, name string, args []string, stdinInput string) (string, string, int, error)
	ReadFileFn        func(ctx context.Context, path string) (string, error)
	WriteFileFn       func(ctx context.Context, path string, data string) error
	WriteFileDirectFn func(ctx context.Context, path string, data string) error
	ReadBlockFn       func(ctx context.Context, path string, offset uint64, length uint64) ([]byte, error)
	WriteBlockFn      func(ctx context.Context, path string, offset uint64, data []byte) error
	ReadProtoFn       func(ctx context.Context, path string, target proto.Message) error
	WriteProtoFn      func(ctx context.Context, path string, msg proto.Message) error
}

var _ OsClient = (*FakeOsClient)(nil)

func (f *FakeOsClient) RunCommand(ctx context.Context, name string, args []string, stdinInput string) (string, string, int, error) {
	if f.RunCommandFn != nil {
		return f.RunCommandFn(ctx, name, args, stdinInput)
	}
	return "", "", 0, nil
}

func (f *FakeOsClient) ReadFile(ctx context.Context, path string) (string, error) {
	if f.ReadFileFn != nil {
		return f.ReadFileFn(ctx, path)
	}
	return "", nil
}

func (f *FakeOsClient) WriteFile(ctx context.Context, path string, data string) error {
	if f.WriteFileFn != nil {
		return f.WriteFileFn(ctx, path, data)
	}
	return nil
}

func (f *FakeOsClient) WriteFileDirect(ctx context.Context, path string, data string) error {
	if f.WriteFileDirectFn != nil {
		return f.WriteFileDirectFn(ctx, path, data)
	}
	return nil
}

func (f *FakeOsClient) ReadBlock(ctx context.Context, path string, offset uint64, length uint64) ([]byte, error) {
	if f.ReadBlockFn != nil {
		return f.ReadBlockFn(ctx, path, offset, length)
	}
	return nil, nil
}

func (f *FakeOsClient) WriteBlock(ctx context.Context, path string, offset uint64, data []byte) error {
	if f.WriteBlockFn != nil {
		return f.WriteBlockFn(ctx, path, offset, data)
	}
	return nil
}

func (f *FakeOsClient) ReadProto(ctx context.Context, path string, target proto.Message) error {
	if f.ReadProtoFn != nil {
		return f.ReadProtoFn(ctx, path, target)
	}
	return nil
}

func (f *FakeOsClient) WriteProto(ctx context.Context, path string, msg proto.Message) error {
	if f.WriteProtoFn != nil {
		return f.WriteProtoFn(ctx, path, msg)
	}
	return nil
}
