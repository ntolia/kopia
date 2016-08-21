// Code generated by protoc-gen-go.
// source: internal/proto_test.proto
// DO NOT EDIT!

/*
Package internal is a generated protocol buffer package.

It is generated from these files:
	internal/proto_test.proto

It has these top-level messages:
	TestProto
*/
package internal

import proto "github.com/golang/protobuf/proto"
import fmt "fmt"
import math "math"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

// This is a compile-time assertion to ensure that this generated file
// is compatible with the proto package it is being compiled against.
const _ = proto.ProtoPackageIsVersion1

type TestProto struct {
	Name string `protobuf:"bytes,1,opt,name=name" json:"name,omitempty"`
}

func (m *TestProto) Reset()                    { *m = TestProto{} }
func (m *TestProto) String() string            { return proto.CompactTextString(m) }
func (*TestProto) ProtoMessage()               {}
func (*TestProto) Descriptor() ([]byte, []int) { return fileDescriptor0, []int{0} }

func init() {
	proto.RegisterType((*TestProto)(nil), "kopia.fs.TestProto")
}

var fileDescriptor0 = []byte{
	// 109 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x09, 0x6e, 0x88, 0x02, 0xff, 0xe2, 0x92, 0xcc, 0xcc, 0x2b, 0x49,
	0x2d, 0xca, 0x4b, 0xcc, 0xd1, 0x2f, 0x28, 0xca, 0x2f, 0xc9, 0x8f, 0x2f, 0x49, 0x2d, 0x2e, 0xd1,
	0x03, 0x33, 0x85, 0x38, 0xb2, 0xf3, 0x0b, 0x32, 0x13, 0xf5, 0xd2, 0x8a, 0x95, 0xe4, 0xb9, 0x38,
	0x43, 0x80, 0xe2, 0x01, 0x60, 0x61, 0x21, 0x2e, 0x96, 0xbc, 0xc4, 0xdc, 0x54, 0x09, 0x46, 0x05,
	0x46, 0x0d, 0xce, 0x20, 0x30, 0xdb, 0x49, 0x31, 0x4a, 0x3e, 0x3d, 0xb3, 0x24, 0xa3, 0x34, 0x49,
	0x2f, 0x39, 0x3f, 0x57, 0x1f, 0xac, 0x0f, 0x4a, 0xc2, 0x8c, 0x4f, 0x62, 0x03, 0x1b, 0x6a, 0x0c,
	0x08, 0x00, 0x00, 0xff, 0xff, 0x49, 0x42, 0xd9, 0xa7, 0x71, 0x00, 0x00, 0x00,
}