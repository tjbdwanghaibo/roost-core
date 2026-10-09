package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

type codecEnum int32
type codecExample struct {
	ID    int64 `json:"player_id"`
	Kind  codecEnum
	Bytes []byte
	Empty []byte
	Nil   []byte
	Map   map[string][]int64
	When  time.Time
	Child *codecChild
}
type codecChild struct {
	Name  string
	Count int
}

// 显式自定义编解码仍由类型自己定义；框架不能借用 JSON Marshal 方法猜测内部格式。
type codecCustom struct{ Value string }

func TestCodecWriterByteAndStringUseTheSameHardLimit(t *testing.T) {
	var writer codecWriter
	if _, err := writer.Write(make([]byte, MaxCodecBytes-1)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("xx"); !errors.Is(err, ErrCodecFormat) {
		t.Fatalf("oversized string=%v", err)
	}
	if writer.Len() != MaxCodecBytes-1 {
		t.Fatal("refused write changed buffer")
	}
	if err := writer.WriteByte(0); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteByte(0); !errors.Is(err, ErrCodecFormat) {
		t.Fatalf("oversized byte=%v", err)
	}
}

func (value codecCustom) MarshalMsgpack() ([]byte, error) { return msgpack.Marshal(value.Value) }
func (value *codecCustom) UnmarshalMsgpack(data []byte) error {
	return msgpack.Unmarshal(data, &value.Value)
}

func TestMessagePackCustomTypesAndNilValues(t *testing.T) {
	codec := MessagePackCodec{}
	type envelope struct {
		Value    codecCustom
		Optional *codecCustom
		When     time.Time
		Dynamic  any
	}
	want := envelope{Value: codecCustom{Value: "自定义"}}
	data, err := codec.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got envelope
	if err := codec.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.When.IsZero() || got.Optional != nil || got.Dynamic != nil || got.Value != want.Value {
		t.Fatalf("roundtrip=%+v", got)
	}
}

func codecSample() codecExample {
	return codecExample{ID: math.MaxInt64, Kind: 7, Bytes: []byte{0, 255, 1}, Empty: []byte{}, Map: map[string][]int64{"x": {math.MinInt64, 17}}, When: time.Unix(1791500000, 123456789).UTC(), Child: &codecChild{Name: "角色", Count: 19}}
}

// MessagePack timestamp 保留时刻与纳秒，不传输 Go 的 Location 或单调时钟字段。
func sameCodecExample(got, want codecExample) bool {
	if !got.When.Equal(want.When) {
		return false
	}
	got.When, want.When = time.Time{}, time.Time{}
	return reflect.DeepEqual(got, want)
}

func TestMessagePackCurrentFormatAndOwnership(t *testing.T) {
	codec := MessagePackCodec{}
	want := codecSample()
	data, err := codec.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got codecExample
	if err := codec.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !sameCodecExample(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	var fields map[string]any
	if err := msgpack.Unmarshal(data[len(messagePackHeader):], &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["ID"]; !ok {
		t.Fatal("missing exported field name")
	}
	if _, ok := fields["player_id"]; ok {
		t.Fatal("JSON tag was used as an implicit protocol rename")
	}
	if _, ok := fields["Bytes"].([]byte); !ok {
		t.Fatal("byte payload is not a MessagePack binary")
	}
	want.Bytes[0] = 77
	if got.Bytes[0] != 0 {
		t.Fatal("encoded data borrowed the input payload")
	}
	for i := range data {
		data[i] = 0
	}
	if got.Bytes[0] != 0 || got.Child.Name != "角色" {
		t.Fatal("decoded data borrowed the input wire")
	}
}

func TestMessagePackRefusesOldTruncatedAndOversizedDeclarations(t *testing.T) {
	codec := MessagePackCodec{}
	valid, err := codec.Marshal(codecSample())
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(codecSample())
	cases := map[string][]byte{
		"json":        oldJSON,
		"unversioned": valid[len(messagePackHeader):],
		"future":      append([]byte{'R', 'M', 2}, valid[len(messagePackHeader):]...),
		"truncated":   valid[:len(valid)-1],
		"trailing":    append(bytes.Clone(valid), 0xc0),
		"huge array":  append([]byte(messagePackHeader), 0xdd, 0xff, 0xff, 0xff, 0xff),
		"huge map":    append([]byte(messagePackHeader), 0xdf, 0xff, 0xff, 0xff, 0xff),
		"huge bin":    append([]byte(messagePackHeader), 0xc6, 0xff, 0xff, 0xff, 0xff),
		"reserved":    append([]byte(messagePackHeader), 0xc1),
	}
	deep := append([]byte(messagePackHeader), bytes.Repeat([]byte{0x91}, 65)...)
	cases["deep"] = append(deep, 0xc0)
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var got any
			if err := codec.Unmarshal(data, &got); !errors.Is(err, ErrCodecFormat) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := codec.Marshal(make([]byte, MaxCodecBytes)); !errors.Is(err, ErrCodecFormat) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestMessagePackCodecConcurrentUse(t *testing.T) {
	codec := MessagePackCodec{}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			for range 100 {
				want := codecSample()
				data, err := codec.Marshal(want)
				if err != nil {
					t.Error(err)
					return
				}
				var got codecExample
				if err := codec.Unmarshal(data, &got); err != nil {
					t.Error(err)
					return
				}
				if !sameCodecExample(got, want) {
					t.Error("concurrent codec corrupted a value")
					return
				}
			}
		})
	}
	group.Wait()
}

func BenchmarkInternalMessageCodec(b *testing.B) {
	value := codecSample()
	for _, format := range []string{"messagepack", "json"} {
		b.Run(format, func(b *testing.B) {
			encode := MessagePackCodec{}.Marshal
			decode := MessagePackCodec{}.Unmarshal
			if format == "json" {
				encode, decode = json.Marshal, json.Unmarshal
			}
			data, err := encode(value)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(data)), "wire-B/op")
			b.Run("encode", func(b *testing.B) {
				for b.Loop() {
					if _, err := encode(value); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(data)), "wire-B/op")
			})
			b.Run("decode", func(b *testing.B) {
				for b.Loop() {
					var result codecExample
					if err := decode(data, &result); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(data)), "wire-B/op")
			})
		})
	}
}
