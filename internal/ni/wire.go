// Ported from ni/grpc-device generated/nidaqmx/nidaqmx.proto and ni/ni-apis
// ni/grpcdevice/v1/session.proto, at v2.20.0 (MIT), Copyright (c) 2022, National
// Instruments Corp. Modified: only the messages Flow calls, written and read by
// hand in Go with NI's field numbers and enum values.

package ni

import (
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

// The messages below are written and read by hand, with the same field numbers
// NI's .proto files give them, because the generated protobuf code and its
// runtime add about 1.7 MB to a binary that has half a megabyte to spare. The
// trimmed .proto files in the proto directory are the generated half of the
// proof: the tests make every message with the generated code and with this
// file and require the same bytes both ways, and compare the numbers here to
// NI's own file.

// NI's enum values, from nidaqmx.proto and session.proto.
const (
	sessionInitializeNew = 1

	acquisitionFinite     = 10178
	acquisitionContinuous = 10123

	edgeRising  = 10280
	edgeFalling = 10171

	termDefault    = -1
	termRSE        = 10083
	termNRSE       = 10078
	termDiff       = 10106
	termPseudoDiff = 12529

	voltageUnitsVolts = 10348
	currentUnitsAmps  = 10342
	shuntDefault      = -1

	tempC = 10143
	tempF = 10144
	tempK = 10325
	tempR = 10145

	tcJ = 10072
	tcK = 10073
	tcN = 10077
	tcR = 10082
	tcS = 10085
	tcT = 10086
	tcB = 10047
	tcE = 10055

	cjcBuiltIn = 10200

	groupByChannel = 0
)

// enc builds one message.
type enc []byte

func (e *enc) str(n protowire.Number, s string) {
	if s != "" {
		*e = protowire.AppendTag(*e, n, protowire.BytesType)
		*e = protowire.AppendString(*e, s)
	}
}

func (e *enc) msg(n protowire.Number, m enc) {
	*e = protowire.AppendTag(*e, n, protowire.BytesType)
	*e = protowire.AppendBytes(*e, m)
}

// varint writes an int32, uint32 or enum the way protobuf does, a negative one
// as 64 bits. A field of a oneof is written even at zero, so always is true.
func (e *enc) varint(n protowire.Number, v int64, always bool) {
	if v != 0 || always {
		*e = protowire.AppendTag(*e, n, protowire.VarintType)
		*e = protowire.AppendVarint(*e, uint64(v))
	}
}

func (e *enc) uvarint(n protowire.Number, v uint64) {
	if v != 0 {
		*e = protowire.AppendTag(*e, n, protowire.VarintType)
		*e = protowire.AppendVarint(*e, v)
	}
}

func (e *enc) f64(n protowire.Number, v float64) {
	if v != 0 || math.Signbit(v) {
		*e = protowire.AppendTag(*e, n, protowire.Fixed64Type)
		*e = protowire.AppendFixed64(*e, math.Float64bits(v))
	}
}

func session(name string) enc {
	var e enc
	e.str(1, name)
	return e
}

func encCreateTask(name string) []byte {
	var e enc
	e.str(1, name)
	e.varint(2, sessionInitializeNew, false)
	return e
}

type aiCommon struct {
	task, physical, name string
	terminal             int32
	min, max             float64
}

func encVoltage(c aiCommon) []byte {
	var e enc
	e.msg(1, session(c.task))
	e.str(2, c.physical)
	e.str(3, c.name)
	e.varint(4, int64(c.terminal), true)
	e.f64(6, c.min)
	e.f64(7, c.max)
	e.varint(8, voltageUnitsVolts, true)
	return e
}

func encCurrent(c aiCommon) []byte {
	var e enc
	e.msg(1, session(c.task))
	e.str(2, c.physical)
	e.str(3, c.name)
	e.varint(4, int64(c.terminal), true)
	e.f64(6, c.min)
	e.f64(7, c.max)
	e.varint(8, currentUnitsAmps, true)
	e.varint(10, shuntDefault, true)
	return e
}

func encThermocouple(c aiCommon, unit, tcType int32) []byte {
	var e enc
	e.msg(1, session(c.task))
	e.str(2, c.physical)
	e.str(3, c.name)
	e.f64(4, c.min)
	e.f64(5, c.max)
	e.varint(6, int64(unit), true)
	e.varint(8, int64(tcType), true)
	e.varint(10, cjcBuiltIn, true)
	return e
}

func encSampClk(task, source string, rate float64, mode int32, falling bool, samples uint64) []byte {
	edge := int64(edgeRising)
	if falling {
		edge = edgeFalling
	}
	var e enc
	e.msg(1, session(task))
	e.str(2, source)
	e.f64(3, rate)
	e.varint(4, edge, true)
	e.varint(6, int64(mode), true)
	e.uvarint(8, samples)
	return e
}

func encStartTrig(task, source string, falling bool) []byte {
	edge := int64(edgeRising)
	if falling {
		edge = edgeFalling
	}
	var e enc
	e.msg(1, session(task))
	e.str(2, source)
	e.varint(3, edge, true)
	return e
}

func encTask(task string) []byte {
	var e enc
	e.msg(1, session(task))
	return e
}

func encRead(task string, n int32, timeout float64, arraySize uint32) []byte {
	var e enc
	e.msg(1, session(task))
	e.varint(2, int64(n), false)
	e.f64(3, timeout)
	e.varint(4, groupByChannel, true)
	e.uvarint(6, uint64(arraySize))
	return e
}

// walk calls fn for each field of a message, skipping the ones fn leaves alone.
func walk(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return protowire.ParseError(m)
		}
		if err := fn(num, typ, b[:m]); err != nil {
			return err
		}
		b = b[m:]
	}
	return nil
}

func varintOf(typ protowire.Type, v []byte) (uint64, bool) {
	if typ != protowire.VarintType {
		return 0, false
	}
	x, n := protowire.ConsumeVarint(v)
	return x, n > 0
}

// decStatus reads field 1, the DAQmx status every response starts with, and
// the session of a CreateTask response.
func decStatus(b []byte) (status int32, taskName string, err error) {
	err = walk(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch num {
		case 1:
			if x, ok := varintOf(typ, v); ok {
				status = int32(x)
			}
		case 2:
			if typ == protowire.BytesType {
				body, _ := protowire.ConsumeBytes(v)
				return walk(body, func(n protowire.Number, t protowire.Type, sv []byte) error {
					if n == 1 && t == protowire.BytesType {
						s, _ := protowire.ConsumeBytes(sv)
						taskName = string(s)
					}
					return nil
				})
			}
		}
		return nil
	})
	return
}

type readReply struct {
	status  int32
	values  []float64
	perChan int32
}

func decRead(b []byte) (r readReply, err error) {
	err = walk(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch num {
		case 1:
			if x, ok := varintOf(typ, v); ok {
				r.status = int32(x)
			}
		case 2:
			switch typ {
			case protowire.BytesType: // packed, which is how proto3 writes it
				body, _ := protowire.ConsumeBytes(v)
				if len(body)%8 != 0 {
					return errors.New("the sample array is not a whole number of doubles")
				}
				for len(body) > 0 {
					x, n := protowire.ConsumeFixed64(body)
					if n < 0 {
						return protowire.ParseError(n)
					}
					r.values = append(r.values, math.Float64frombits(x))
					body = body[n:]
				}
			case protowire.Fixed64Type: // one at a time, which a decoder must accept too
				x, _ := protowire.ConsumeFixed64(v)
				r.values = append(r.values, math.Float64frombits(x))
			}
		case 3:
			if x, ok := varintOf(typ, v); ok {
				r.perChan = int32(x)
			}
		}
		return nil
	})
	if err != nil {
		err = fmt.Errorf("the answer is not the message this call expects: %v", err)
	}
	return
}
