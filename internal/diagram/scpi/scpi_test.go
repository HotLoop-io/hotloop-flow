package scpi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestNumbers(t *testing.T) {
	got, err := Numbers("1.5, 2,-3E-2;4")
	if err != nil || fmt.Sprint(got) != "[1.5 2 -0.03 4]" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "1,,2", "1,abc"} {
		if _, err := Numbers(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestIdentity(t *testing.T) {
	id, err := ParseIdentity("ACME,Model 9,SN1,1.2,beta\r\n")
	if err != nil || id.Manufacturer != "ACME" || id.Firmware != "1.2,beta" {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := ParseIdentity("OK"); err == nil {
		t.Fatal("a short reply parsed")
	}
}

func TestStatusBits(t *testing.T) {
	if got := fmt.Sprint(StatusBits(0x14, STB)); got != "[error queue message available]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(StatusBits(0x21, ESR)); got != "[operation complete command error]" {
		t.Fatal(got)
	}
}

// The case from the spec: #41024 and 1024 bytes are 512 sixteen bit samples.
func TestDefiniteBlockOfSixteenBitSamples(t *testing.T) {
	var body bytes.Buffer
	for i := 0; i < 512; i++ {
		binary.Write(&body, binary.BigEndian, int16(i-256))
	}
	data := append([]byte("#41024"), body.Bytes()...)
	data = append(data, '\n')
	payload, rest, err := Block(data)
	if err != nil || len(payload) != 1024 || string(rest) != "\n" {
		t.Fatalf("%d %q %v", len(payload), rest, err)
	}
	s, err := Samples(payload, "int16", false)
	if err != nil || len(s) != 512 || s[0] != -256 || s[511] != 255 {
		t.Fatalf("%d %v", len(s), err)
	}
}

func TestIndefiniteBlockAndLittleEndian(t *testing.T) {
	var body bytes.Buffer
	binary.Write(&body, binary.LittleEndian, float32(1.5))
	binary.Write(&body, binary.LittleEndian, float32(-2))
	p, rest, err := Block(append(append([]byte("#0"), body.Bytes()...), []byte("\nnext")...))
	if err != nil || string(rest) != "next" {
		t.Fatalf("%q %v", rest, err)
	}
	s, _ := Samples(p, "float32", true)
	if fmt.Sprint(s) != "[1.5 -2]" {
		t.Fatal(s)
	}
}

func TestBlockRefusals(t *testing.T) {
	for name, in := range map[string]string{"no hash": "41024", "short": "#41024abc", "bad digit": "#x", "no end": "#0abc", "cut header": "#4"} {
		if _, _, err := Block([]byte(in)); err == nil {
			t.Errorf("%s parsed", name)
		}
	}
	if _, err := Samples([]byte{1, 2, 3}, "int16", false); err == nil {
		t.Error("odd length decoded")
	}
	if _, err := Samples(nil, "bogus", false); err == nil {
		t.Error("bogus format decoded")
	}
}
