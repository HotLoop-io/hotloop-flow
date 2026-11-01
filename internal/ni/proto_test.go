package ni

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	pb "github.com/HotLoop-io/hotloop-flow/internal/ni/proto"
)

const vendored = "../../third_party/ni-grpc-device"

var (
	niOnce  sync.Once
	niFiles protoreflect.FileDescriptor
	niErr   error
)

// niVerbatim compiles NI's own nidaqmx.proto, as vendored, byte for byte.
func niVerbatim(t testing.TB) protoreflect.FileDescriptor {
	t.Helper()
	niOnce.Do(func() {
		c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: []string{
			filepath.Join(vendored, "generated/nidaqmx"),
			filepath.Join(vendored, "imports/protobuf"),
			filepath.Join(vendored, "ni-apis/ni/grpcdevice/v1"),
			filepath.Join(vendored, "ni-apis"),
		}})}
		res, err := c.Compile(context.Background(), "nidaqmx.proto")
		if err != nil {
			niErr = err
			return
		}
		niFiles = res[0]
	})
	if niErr != nil {
		t.Fatalf("NI's nidaqmx.proto does not compile: %v", niErr)
	}
	return niFiles
}

func trimmed(t testing.TB) protoreflect.FileDescriptor {
	t.Helper()
	return pb.File_nidaqmx_proto
}

// diffs lists every way the trimmed file differs from NI's for what it declares.
func diffs(ni, tr protoreflect.FileDescriptor) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	if ni.Package() != tr.Package() {
		add("package %q is %q at NI", tr.Package(), ni.Package())
	}
	findNI := func(full protoreflect.FullName) protoreflect.Descriptor {
		d, err := findTopLevel(ni, full)
		if err != nil {
			return nil
		}
		return d
	}
	msgs := tr.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		nd := findNI(m.FullName())
		nm, ok := nd.(protoreflect.MessageDescriptor)
		if !ok {
			add("message %s is not in NI's file", m.FullName())
			continue
		}
		if m.Fields().Len() != nm.Fields().Len() {
			add("message %s has %d fields, NI's has %d", m.FullName(), m.Fields().Len(), nm.Fields().Len())
		}
		for j := 0; j < m.Fields().Len(); j++ {
			f := m.Fields().Get(j)
			nf := nm.Fields().ByNumber(f.Number())
			if nf == nil {
				add("%s field %s is number %d, which NI's message does not have", m.FullName(), f.Name(), f.Number())
				continue
			}
			switch {
			case nf.Name() != f.Name():
				add("%s field %d is named %s, NI's is %s", m.FullName(), f.Number(), f.Name(), nf.Name())
			case nf.Kind() != f.Kind():
				add("%s.%s is %s, NI's is %s", m.FullName(), f.Name(), f.Kind(), nf.Kind())
			case nf.Cardinality() != f.Cardinality():
				add("%s.%s is %s, NI's is %s", m.FullName(), f.Name(), f.Cardinality(), nf.Cardinality())
			case nf.IsPacked() != f.IsPacked():
				add("%s.%s packed is %v, NI's is %v", m.FullName(), f.Name(), f.IsPacked(), nf.IsPacked())
			}
			if f.Message() != nil && (nf.Message() == nil || nf.Message().FullName() != f.Message().FullName()) {
				add("%s.%s is of message type %s, NI's differs", m.FullName(), f.Name(), f.Message().FullName())
			}
			if f.Enum() != nil && (nf.Enum() == nil || nf.Enum().FullName() != f.Enum().FullName()) {
				add("%s.%s is of enum type %s, NI's differs", m.FullName(), f.Name(), f.Enum().FullName())
			}
			po, no := f.ContainingOneof(), nf.ContainingOneof()
			if (po == nil) != (no == nil) || (po != nil && po.Name() != no.Name()) {
				add("%s.%s sits in a different oneof than NI's", m.FullName(), f.Name())
			}
		}
	}
	enums := tr.Enums()
	for i := 0; i < enums.Len(); i++ {
		e := enums.Get(i)
		ne, ok := findNI(e.FullName()).(protoreflect.EnumDescriptor)
		if !ok {
			add("enum %s is not in NI's file", e.FullName())
			continue
		}
		if e.Values().Len() != ne.Values().Len() {
			add("enum %s has %d values, NI's has %d", e.FullName(), e.Values().Len(), ne.Values().Len())
		}
		for j := 0; j < e.Values().Len(); j++ {
			v := e.Values().Get(j)
			nv := ne.Values().ByName(v.Name())
			if nv == nil || nv.Number() != v.Number() {
				add("enum value %s = %d is not NI's", v.Name(), v.Number())
			}
		}
	}
	svcs := tr.Services()
	for i := 0; i < svcs.Len(); i++ {
		s := svcs.Get(i)
		ns, ok := findNI(s.FullName()).(protoreflect.ServiceDescriptor)
		if !ok {
			add("service %s is not in NI's file", s.FullName())
			continue
		}
		for j := 0; j < s.Methods().Len(); j++ {
			m := s.Methods().Get(j)
			nm := ns.Methods().ByName(m.Name())
			switch {
			case nm == nil:
				add("method %s.%s is not NI's", s.FullName(), m.Name())
			case nm.Input().FullName() != m.Input().FullName() || nm.Output().FullName() != m.Output().FullName():
				add("method %s takes %s and returns %s, NI's takes %s and returns %s", m.FullName(), m.Input().FullName(), m.Output().FullName(), nm.Input().FullName(), nm.Output().FullName())
			case nm.IsStreamingClient() || nm.IsStreamingServer():
				add("method %s streams at NI", m.FullName())
			}
		}
	}
	// session.proto, which nidaqmx.proto imports, is NI's too.
	sess := pb.File_session_proto
	niSess := importOf(ni, "session.proto")
	if niSess == nil {
		add("NI's file does not import session.proto")
	} else {
		for i := 0; i < sess.Messages().Len(); i++ {
			m := sess.Messages().Get(i)
			nm := niSess.Messages().ByName(m.Name())
			if nm == nil || nm.Fields().Len() != m.Fields().Len() {
				add("session message %s is not NI's", m.Name())
				continue
			}
			for j := 0; j < m.Fields().Len(); j++ {
				f, nf := m.Fields().Get(j), nm.Fields().ByNumber(m.Fields().Get(j).Number())
				if nf == nil || nf.Name() != f.Name() || nf.Kind() != f.Kind() {
					add("session message %s field %s is not NI's", m.Name(), f.Name())
				}
			}
		}
		for i := 0; i < sess.Enums().Len(); i++ {
			e := sess.Enums().Get(i)
			ne := niSess.Enums().ByName(e.Name())
			if ne == nil || ne.Values().Len() != e.Values().Len() {
				add("session enum %s is not NI's", e.Name())
				continue
			}
			for j := 0; j < e.Values().Len(); j++ {
				v := e.Values().Get(j)
				if nv := ne.Values().ByName(v.Name()); nv == nil || nv.Number() != v.Number() {
					add("session enum value %s is not NI's", v.Name())
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func importOf(fd protoreflect.FileDescriptor, path string) protoreflect.FileDescriptor {
	for i := 0; i < fd.Imports().Len(); i++ {
		if fd.Imports().Get(i).Path() == path {
			return fd.Imports().Get(i).FileDescriptor
		}
	}
	return nil
}

// findTopLevel finds a top level message, enum or service by full name in
// a file.
func findTopLevel(fd protoreflect.FileDescriptor, full protoreflect.FullName) (protoreflect.Descriptor, error) {
	name := strings.TrimPrefix(string(full), string(fd.Package())+".")
	if d := fd.Messages().ByName(protoreflect.Name(name)); d != nil {
		return d, nil
	}
	if d := fd.Enums().ByName(protoreflect.Name(name)); d != nil {
		return d, nil
	}
	if d := fd.Services().ByName(protoreflect.Name(name)); d != nil {
		return d, nil
	}
	return nil, fmt.Errorf("%s not found", full)
}

// TestTrimmedMatchesNI is the wire compatibility proof for the generated half:
// every message, field number, name, type and oneof, every enum value and every
// service method in the trimmed files equals NI's own, compiled from the
// vendored original.
func TestTrimmedMatchesNI(t *testing.T) {
	ni, tr := niVerbatim(t), trimmed(t)
	if n := tr.Messages().Len(); n < 20 {
		t.Fatalf("only %d messages in the trimmed file; the comparison would prove little", n)
	}
	if d := diffs(ni, tr); len(d) > 0 {
		t.Fatalf("the trimmed file drifted from NI's:\n  %s", strings.Join(d, "\n  "))
	}
	t.Logf("%d messages, %d enums, %d methods compared against NI's %d messages", tr.Messages().Len(), tr.Enums().Len(), tr.Services().Get(0).Methods().Len(), ni.Messages().Len())
}

// TestComparerCatchesDrift keeps the comparison honest: change one field number,
// one name and one enum value in a copy of the trimmed file, and each is found.
func TestComparerCatchesDrift(t *testing.T) {
	ni := niVerbatim(t)
	fdp := protodesc.ToFileDescriptorProto(trimmed(t))
	cp := proto.Clone(fdp).(*descriptorpb.FileDescriptorProto)
	for _, m := range cp.MessageType {
		switch m.GetName() {
		case "ReadAnalogF64Response":
			m.Field[2].Number = proto.Int32(4) // samps_per_chan_read 3 -> 4
		case "CfgSampClkTimingRequest":
			m.Field[1].Name = proto.String("src")
		}
	}
	for _, e := range cp.EnumType {
		if e.GetName() == "AcquisitionType" {
			e.Value[1].Number = proto.Int32(10179)
		}
	}
	bad, err := protodesc.NewFile(cp, fileResolver{})
	if err != nil {
		t.Fatal(err)
	}
	d := diffs(ni, bad)
	want := []string{"samps_per_chan_read", "src", "10179"}
	for _, w := range want {
		found := false
		for _, line := range d {
			if strings.Contains(line, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("the drift involving %q was not reported; reported: %v", w, d)
		}
	}
}

// fileResolver resolves the imports of the trimmed file (session.proto).
type fileResolver struct{}

func (r fileResolver) FindFileByPath(p string) (protoreflect.FileDescriptor, error) {
	if p == "session.proto" {
		return pb.File_session_proto, nil
	}
	return nil, fmt.Errorf("no file %s", p)
}

func (r fileResolver) FindDescriptorByName(n protoreflect.FullName) (protoreflect.Descriptor, error) {
	if d, err := findTopLevel(pb.File_session_proto, n); err == nil {
		return d, nil
	}
	return nil, fmt.Errorf("no descriptor %s", n)
}

// TestEnumValuesAreNIs checks the constants wire.go sends against NI's file.
func TestEnumValuesAreNIs(t *testing.T) {
	ni := niVerbatim(t)
	sess := importOf(ni, "session.proto")
	cases := []struct {
		enum, value string
		got         int32
	}{
		{"AcquisitionType", "ACQUISITION_TYPE_FINITE_SAMPS", acquisitionFinite},
		{"AcquisitionType", "ACQUISITION_TYPE_CONT_SAMPS", acquisitionContinuous},
		{"Edge1", "EDGE1_RISING", edgeRising},
		{"Edge1", "EDGE1_FALLING", edgeFalling},
		{"InputTermCfgWithDefault", "INPUT_TERM_CFG_WITH_DEFAULT_CFG_DEFAULT", termDefault},
		{"InputTermCfgWithDefault", "INPUT_TERM_CFG_WITH_DEFAULT_RSE", termRSE},
		{"InputTermCfgWithDefault", "INPUT_TERM_CFG_WITH_DEFAULT_NRSE", termNRSE},
		{"InputTermCfgWithDefault", "INPUT_TERM_CFG_WITH_DEFAULT_DIFF", termDiff},
		{"InputTermCfgWithDefault", "INPUT_TERM_CFG_WITH_DEFAULT_PSEUDO_DIFF", termPseudoDiff},
		{"VoltageUnits2", "VOLTAGE_UNITS2_VOLTS", voltageUnitsVolts},
		{"CurrentUnits2", "CURRENT_UNITS2_AMPS", currentUnitsAmps},
		{"CurrentShuntResistorLocationWithDefault", "CURRENT_SHUNT_RESISTOR_LOCATION_WITH_DEFAULT_DEFAULT", shuntDefault},
		{"TemperatureUnits", "TEMPERATURE_UNITS_DEG_C", tempC},
		{"TemperatureUnits", "TEMPERATURE_UNITS_DEG_F", tempF},
		{"TemperatureUnits", "TEMPERATURE_UNITS_KELVINS", tempK},
		{"TemperatureUnits", "TEMPERATURE_UNITS_DEG_R", tempR},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_J_TYPE_TC", tcJ},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_K_TYPE_TC", tcK},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_N_TYPE_TC", tcN},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_R_TYPE_TC", tcR},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_S_TYPE_TC", tcS},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_T_TYPE_TC", tcT},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_B_TYPE_TC", tcB},
		{"ThermocoupleType1", "THERMOCOUPLE_TYPE1_E_TYPE_TC", tcE},
		{"CJCSource1", "CJC_SOURCE1_BUILT_IN", cjcBuiltIn},
		{"GroupBy", "GROUP_BY_GROUP_BY_CHANNEL", groupByChannel},
	}
	for _, c := range cases {
		e := ni.Enums().ByName(protoreflect.Name(c.enum))
		if e == nil {
			t.Errorf("NI has no enum %s", c.enum)
			continue
		}
		v := e.Values().ByName(protoreflect.Name(c.value))
		if v == nil {
			t.Errorf("%s is not in NI's file", c.value)
		} else if int32(v.Number()) != c.got {
			t.Errorf("%s: wire.go sends %d, NI's file says %d", c.value, c.got, v.Number())
		}
	}
	if v := sess.Enums().ByName("SessionInitializationBehavior").Values().ByName("SESSION_INITIALIZATION_BEHAVIOR_INITIALIZE_NEW"); v == nil || v.Number() != sessionInitializeNew {
		t.Errorf("session initialize new is not %d in NI's file", sessionInitializeNew)
	}
}

// read decodes bytes with NI's own (verbatim) descriptor and returns the message.
func readNI(t *testing.T, name string, b []byte) *dynamicpb.Message {
	t.Helper()
	md := niVerbatim(t).Messages().ByName(protoreflect.Name(name))
	if md == nil {
		t.Fatalf("NI has no message %s", name)
	}
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(b, m); err != nil {
		t.Fatalf("NI's %s cannot read what wire.go wrote: %v", name, err)
	}
	return m
}

func field(m *dynamicpb.Message, name string) protoreflect.Value {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		panic("no field " + name)
	}
	return m.Get(fd)
}

func has(m *dynamicpb.Message, name string) bool {
	return m.Has(m.Descriptor().Fields().ByName(protoreflect.Name(name)))
}

func taskName(m *dynamicpb.Message) string {
	return field(m, "task").Message().Get(field(m, "task").Message().Descriptor().Fields().ByName("name")).String()
}

// TestHandCodecRequestsReadByNI writes every request with wire.go and reads it
// with NI's own descriptors, and with the generated types, and checks the values.
func TestHandCodecRequestsReadByNI(t *testing.T) {
	c := aiCommon{task: "flow-1", physical: "cDAQ1Mod1/ai0:1", name: "pressure", terminal: termDiff, min: -10, max: 10.5}
	t.Run("CreateTask", func(t *testing.T) {
		m := readNI(t, "CreateTaskRequest", encCreateTask("flow-1"))
		if field(m, "session_name").String() != "flow-1" || field(m, "initialization_behavior").Enum() != sessionInitializeNew {
			t.Errorf("read back %v", m)
		}
		var g pb.CreateTaskRequest
		if err := proto.Unmarshal(encCreateTask("flow-1"), &g); err != nil || g.SessionName != "flow-1" || g.InitializationBehavior != pb.SessionInitializationBehavior_SESSION_INITIALIZATION_BEHAVIOR_INITIALIZE_NEW {
			t.Errorf("generated read back %v %v", &g, err)
		}
	})
	t.Run("CreateAIVoltageChan", func(t *testing.T) {
		b := encVoltage(c)
		m := readNI(t, "CreateAIVoltageChanRequest", b)
		if taskName(m) != "flow-1" || field(m, "physical_channel").String() != "cDAQ1Mod1/ai0:1" || field(m, "name_to_assign_to_channel").String() != "pressure" ||
			field(m, "terminal_config").Enum() != termDiff || field(m, "min_val").Float() != -10 || field(m, "max_val").Float() != 10.5 || field(m, "units").Enum() != voltageUnitsVolts {
			t.Errorf("read back %v", m)
		}
		var g pb.CreateAIVoltageChanRequest
		if err := proto.Unmarshal(b, &g); err != nil || g.GetUnits() != pb.VoltageUnits2_VOLTAGE_UNITS2_VOLTS || g.GetTerminalConfig() != pb.InputTermCfgWithDefault_INPUT_TERM_CFG_WITH_DEFAULT_DIFF || g.MinVal != -10 {
			t.Errorf("generated read back %v %v", &g, err)
		}
	})
	t.Run("CreateAICurrentChan", func(t *testing.T) {
		m := readNI(t, "CreateAICurrentChanRequest", encCurrent(c))
		if taskName(m) != "flow-1" || field(m, "units").Enum() != currentUnitsAmps || field(m, "shunt_resistor_loc").Enum() != shuntDefault ||
			field(m, "terminal_config").Enum() != termDiff || field(m, "max_val").Float() != 10.5 || field(m, "physical_channel").String() != "cDAQ1Mod1/ai0:1" {
			t.Errorf("read back %v", m)
		}
	})
	t.Run("CreateAIThrmcplChan", func(t *testing.T) {
		m := readNI(t, "CreateAIThrmcplChanRequest", encThermocouple(aiCommon{task: "flow-1", physical: "cDAQ1Mod2/ai3", name: "oven", min: 0, max: 400}, tempF, tcK))
		if taskName(m) != "flow-1" || field(m, "units").Enum() != tempF || field(m, "thermocouple_type").Enum() != tcK || field(m, "cjc_source").Enum() != cjcBuiltIn ||
			field(m, "max_val").Float() != 400 || field(m, "physical_channel").String() != "cDAQ1Mod2/ai3" {
			t.Errorf("read back %v", m)
		}
	})
	t.Run("CfgSampClkTiming", func(t *testing.T) {
		m := readNI(t, "CfgSampClkTimingRequest", encSampClk("flow-1", "/cDAQ1/PFI1", 1000, acquisitionContinuous, true, 5000))
		if taskName(m) != "flow-1" || field(m, "source").String() != "/cDAQ1/PFI1" || field(m, "rate").Float() != 1000 || field(m, "active_edge").Enum() != edgeFalling ||
			field(m, "sample_mode").Enum() != acquisitionContinuous || field(m, "samps_per_chan").Uint() != 5000 {
			t.Errorf("read back %v", m)
		}
	})
	t.Run("CfgDigEdgeStartTrig", func(t *testing.T) {
		m := readNI(t, "CfgDigEdgeStartTrigRequest", encStartTrig("flow-1", "/cDAQ1/PFI0", false))
		if taskName(m) != "flow-1" || field(m, "trigger_source").String() != "/cDAQ1/PFI0" || field(m, "trigger_edge").Enum() != edgeRising || !has(m, "trigger_edge") {
			t.Errorf("read back %v", m)
		}
	})
	t.Run("Start Stop Clear", func(t *testing.T) {
		for _, n := range []string{"StartTaskRequest", "StopTaskRequest", "ClearTaskRequest"} {
			if m := readNI(t, n, encTask("flow-1")); taskName(m) != "flow-1" {
				t.Errorf("%s read back %v", n, m)
			}
		}
	})
	t.Run("ReadAnalogF64", func(t *testing.T) {
		b := encRead("flow-1", 1000, 12.5, 2000)
		m := readNI(t, "ReadAnalogF64Request", b)
		if taskName(m) != "flow-1" || field(m, "num_samps_per_chan").Int() != 1000 || field(m, "timeout").Float() != 12.5 || field(m, "array_size_in_samps").Uint() != 2000 ||
			!has(m, "fill_mode") || field(m, "fill_mode").Enum() != groupByChannel {
			t.Errorf("read back %v", m)
		}
		var g pb.ReadAnalogF64Request
		if err := proto.Unmarshal(b, &g); err != nil {
			t.Fatal(err)
		}
		if _, set := g.GetFillModeEnum().(*pb.ReadAnalogF64Request_FillMode); !set {
			t.Error("the fill mode is not set on the wire, and NI's server refuses a read without it")
		}
	})
}

// TestHandCodecReadsNIResponses writes responses with NI's descriptors (and
// with the generated types) and reads them with wire.go.
func TestHandCodecReadsNIResponses(t *testing.T) {
	vals := []float64{0, 1.5, -2.25, math.Pi, math.Inf(1)}
	md := niVerbatim(t).Messages().ByName("ReadAnalogF64Response")
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("status"), protoreflect.ValueOfInt32(0))
	l := m.Mutable(md.Fields().ByName("read_array")).List()
	for _, v := range vals {
		l.Append(protoreflect.ValueOfFloat64(v))
	}
	m.Set(md.Fields().ByName("samps_per_chan_read"), protoreflect.ValueOfInt32(5))
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r, err := decRead(b)
	if err != nil || r.perChan != 5 || len(r.values) != len(vals) {
		t.Fatalf("read %+v %v", r, err)
	}
	for i := range vals {
		if r.values[i] != vals[i] {
			t.Errorf("value %d: %v, want %v", i, r.values[i], vals[i])
		}
	}
	g, _ := proto.Marshal(&pb.ReadAnalogF64Response{Status: 1, ReadArray: vals, SampsPerChanRead: 5})
	if r, err := decRead(g); err != nil || r.status != 1 || len(r.values) != 5 {
		t.Errorf("generated: %+v %v", r, err)
	}
	// An unpacked array, which a proto3 reader must accept, is read too.
	var un []byte
	for _, v := range vals[:2] {
		un = append(un, 0x11) // field 2, fixed64
		bits := math.Float64bits(v)
		for i := 0; i < 8; i++ {
			un = append(un, byte(bits>>(8*i)))
		}
	}
	if r, err := decRead(un); err != nil || len(r.values) != 2 || r.values[1] != 1.5 {
		t.Errorf("unpacked: %+v %v", r, err)
	}
	// CreateTask's answer: the session name comes out of a nested message.
	cd := niVerbatim(t).Messages().ByName("CreateTaskResponse")
	cm := dynamicpb.NewMessage(cd)
	sd := cd.Fields().ByName("task").Message()
	sm := dynamicpb.NewMessage(sd)
	sm.Set(sd.Fields().ByName("name"), protoreflect.ValueOfString("flow-9"))
	cm.Set(cd.Fields().ByName("task"), protoreflect.ValueOfMessage(sm))
	cm.Set(cd.Fields().ByName("new_session_initialized"), protoreflect.ValueOfBool(true))
	cb, _ := proto.Marshal(cm)
	if st, name, err := decStatus(cb); err != nil || name != "flow-9" || st != 0 {
		t.Errorf("CreateTask answer: %d %q %v", st, name, err)
	}
	// A negative warning or error status is a 10 byte varint.
	nb, _ := proto.Marshal(&pb.StartTaskResponse{Status: -200220})
	if st, _, err := decStatus(nb); err != nil || st != -200220 {
		t.Errorf("negative status: %d %v", st, err)
	}
	// Garbage is an error, not a panic.
	if _, err := decRead([]byte{0x12, 0x09, 1, 2}); err == nil {
		t.Error("a truncated array was accepted")
	}
}

func TestChannelCount(t *testing.T) {
	for in, want := range map[string]int{"cDAQ1Mod1/ai0": 1, "cDAQ1Mod1/ai0:3": 4, "cDAQ1Mod1/ai3:0": 4, "cDAQ1Mod1/ai0, cDAQ1Mod2/ai4:5": 3, "Dev1/ai10:12": 3} {
		if got, err := channelCount(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a,,b", "dev/aiX:Y", "dev/ai0:"} {
		if _, err := channelCount(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestDecodeGRPCMessage(t *testing.T) {
	for in, want := range map[string]string{"plain": "plain", "a%20b": "a b", "100%": "100%", "bad%zz": "bad%zz", "%E2%9C%93": "\u2713", "tail%2": "tail%2"} {
		if got := decodeGRPCMessage(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
