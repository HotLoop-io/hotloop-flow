// Package proto holds the Go types for the NI gRPC Device Server messages that
// Flow's DAQ client sends and reads. They are made from the trimmed .proto files
// in this directory, which are derived from NI's (see the headers).
package proto

//go:generate go run generate.go
