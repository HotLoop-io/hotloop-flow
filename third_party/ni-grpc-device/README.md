# NI gRPC Device Server protos, kept as NI wrote them

These files are copied byte for byte from two NI projects. Nothing in here is
built into Flow. They are here for two reasons: the MIT license asks that the
copyright line and permission text travel with the work, and
`internal/ni/proto_test.go` compiles them to prove that the cut-down copies in
`internal/ni/proto/` and the hand-written messages in `internal/ni/wire.go` put
the same bytes on the wire as NI's.

| File here | Comes from | At |
|---|---|---|
| `generated/nidaqmx/nidaqmx.proto` | https://github.com/ni/grpc-device, `generated/nidaqmx/nidaqmx.proto` | tag `v2.20.0`, commit `65f4e23238e38525963996eac4f51814226c551e` |
| `imports/protobuf/data_moniker.proto` | https://github.com/ni/grpc-device, `imports/protobuf/data_moniker.proto` | tag `v2.20.0`, same commit |
| `LICENSE` | https://github.com/ni/grpc-device, `LICENSE` | tag `v2.20.0`, same commit |
| `ni-apis/ni/grpcdevice/v1/session.proto` | https://github.com/ni/ni-apis, `ni/grpcdevice/v1/session.proto` | commit `00356cce09dd61d15f6799a87c27460a7d7a0c24`, the revision grpc-device v2.20.0 pins as its `third_party/ni-apis` |
| `ni-apis/ni/protobuf/types/waveform.proto`, `precision_timestamp.proto` | https://github.com/ni/ni-apis, same paths | same commit |
| `ni-apis/LICENSE` | https://github.com/ni/ni-apis, `LICENSE` | same commit |

Fetched 2026-11-01. License: MIT, Copyright (c) 2022, National Instruments Corp.
The full text is in `LICENSE`, and in the repository's `NOTICE`.

SHA-256 of each file as fetched:

```
e87367cb36b82dfe5e526dc733387a6d72f7b189138edf2d9e6113275ac45fab  generated/nidaqmx/nidaqmx.proto
ef6280eb4b41c0ac7a025b1b5c625dffd7b35f9cd1ba1132aed01658a21e9b28  imports/protobuf/data_moniker.proto
5fb6727684bd4a4273e7747756e1e56a096d3fe01693bf42a07d9d7c5ffa123e  ni-apis/ni/grpcdevice/v1/session.proto
1a89554442345da0b66c0608072e4bbf7d9dda71295c11da42b4739ea83b2c9f  ni-apis/ni/protobuf/types/waveform.proto
70f696656136680ab1ad5e3c10b2f6cb9049ef63c7012b2eb4f4b134c4d488d0  ni-apis/ni/protobuf/types/precision_timestamp.proto
```

To move to a newer NI release, replace these files from the new tag, update the
table, and run `go test ./internal/ni/...`: the wire test fails if NI changed a
field number, name or type that Flow uses.
