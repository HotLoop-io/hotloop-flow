// Package scpi parses what test instruments send back over a text protocol: a
// comma list of numbers, an identification reply, a status byte, and the binary
// block (IEEE 488.2 definite and indefinite length) that scopes and meters use
// for waveforms. Every function is pure.
package scpi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Numbers parses a reply such as "1.5,2,-3E-2" or "1.5; 2" into numbers. Blank
// entries are an error, because an instrument that sends one has lost a value.
func Numbers(reply string) ([]float64, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil, errors.New("the reply is empty")
	}
	parts := strings.Split(strings.ReplaceAll(reply, ";", ","), ",")
	out := make([]float64, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("entry %d of the reply is blank", i+1)
		}
		// Instruments spell out not-a-number and infinity as 9.91E37 and friends;
		// those parse as ordinary numbers and the caller decides what they mean.
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("entry %d, %q, is not a number", i+1, p)
		}
		out = append(out, f)
	}
	return out, nil
}

// Identity is the four fields of a *IDN? reply.
type Identity struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	Firmware     string `json:"firmware"`
}

// ParseIdentity reads "maker,model,serial,firmware". A reply with fewer fields is
// an error, since a reply that is not an identity usually means the wrong port.
func ParseIdentity(reply string) (Identity, error) {
	f := strings.Split(strings.TrimSpace(reply), ",")
	if len(f) < 4 {
		return Identity{}, fmt.Errorf("%q is not an identification reply: it has %d of 4 fields", reply, len(f))
	}
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	return Identity{f[0], f[1], f[2], strings.Join(f[3:], ",")}, nil
}

// StatusBits names the bits set in a status byte (*STB? or *ESR?).
func StatusBits(v int, names [8]string) []string {
	var out []string
	for i := 0; i < 8; i++ {
		if v&(1<<i) != 0 {
			out = append(out, names[i])
		}
	}
	return out
}

// STB and ESR are the standard names of the bits of the two status bytes.
var (
	STB = [8]string{"", "", "error queue", "questionable", "message available", "event status", "master summary", "operation"}
	ESR = [8]string{"operation complete", "request control", "query error", "device error", "execution error", "command error", "user request", "power on"}
)

// Block splits a binary block off the front of data. It returns the payload and
// whatever follows. "#41024..." is a definite block of 1024 bytes; "#0..." is
// indefinite and ends at the first newline.
func Block(data []byte) (payload, rest []byte, err error) {
	if len(data) < 2 || data[0] != '#' {
		return nil, nil, errors.New("a binary block starts with #")
	}
	digits := int(data[1] - '0')
	if data[1] < '0' || data[1] > '9' {
		return nil, nil, fmt.Errorf("%q is not a digit count after #", data[1])
	}
	if digits == 0 {
		for i := 2; i < len(data); i++ {
			if data[i] == '\n' {
				return data[2:i], data[i+1:], nil
			}
		}
		return nil, nil, errors.New("an indefinite block has no end of line")
	}
	if len(data) < 2+digits {
		return nil, nil, errors.New("the block header is cut short")
	}
	n, err := strconv.Atoi(string(data[2 : 2+digits]))
	if err != nil || n < 0 {
		return nil, nil, fmt.Errorf("%q is not a block length", data[2:2+digits])
	}
	start := 2 + digits
	if len(data) < start+n {
		return nil, nil, fmt.Errorf("the block says %d bytes and %d came", n, len(data)-start)
	}
	return data[start : start+n], data[start+n:], nil
}

// Samples decodes a payload by format: int16, uint16, int32, float32 or float64,
// big endian unless little is set. A length that is not a whole number of
// samples is an error.
func Samples(payload []byte, format string, little bool) ([]float64, error) {
	var order binary.ByteOrder = binary.BigEndian
	if little {
		order = binary.LittleEndian
	}
	size := map[string]int{"int8": 1, "uint8": 1, "int16": 2, "uint16": 2, "int32": 4, "float32": 4, "float64": 8}[format]
	if size == 0 {
		return nil, fmt.Errorf("%q is not a sample format", format)
	}
	if len(payload)%size != 0 {
		return nil, fmt.Errorf("%d bytes is not a whole number of %s samples", len(payload), format)
	}
	out := make([]float64, len(payload)/size)
	for i := range out {
		b := payload[i*size:]
		switch format {
		case "int8":
			out[i] = float64(int8(b[0]))
		case "uint8":
			out[i] = float64(b[0])
		case "int16":
			out[i] = float64(int16(order.Uint16(b)))
		case "uint16":
			out[i] = float64(order.Uint16(b))
		case "int32":
			out[i] = float64(int32(order.Uint32(b)))
		case "float32":
			out[i] = float64(math.Float32frombits(order.Uint32(b)))
		case "float64":
			out[i] = math.Float64frombits(order.Uint64(b))
		}
	}
	return out, nil
}
