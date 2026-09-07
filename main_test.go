package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseRTT(t *testing.T) {
	for _, tc := range []struct {
		text string
		want float64
	}{
		{"64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=12.345 ms", 12.345},
		{"Reply from 127.0.0.1: bytes=32 time<1ms TTL=128", .5},
		{"Ответ: число байт=32 время=2мс TTL=128", 2},
		{"garbled=32 garbled<1garbled TTL=128", .5},
		// Raw OEM console bytes are not valid UTF-8. Case conversion must
		// not change the byte offsets used to slice the original reply.
		{"\x8e\xe2\xa2\xa5\xe2 8.8.8.8: \xe7\xa8\xe1\xab\xae=32 \xa2\xe0\xa5\xac\xef=24\xac\xe1 TTL=115", 24},
		{"\xff\xfe=32 \x80\x81<1\xac\xe1 ttl=128", .5},
		{"64 bytes: time=1,25 ms", 1.25},
	} {
		got, err := parseRTT(tc.text)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %v, %v; want %v", tc.text, got, err, tc.want)
		}
	}
	for _, text := range []string{"Request timed out.", "Destination host unreachable.", "Minimum = 0ms, Maximum = 0ms, Average = 0ms"} {
		if _, err := parseRTT(text); err == nil {
			t.Errorf("accepted failure: %q", text)
		}
	}
}

func TestPercentile(t *testing.T) {
	if got := percentile([]float64{10, 20, 30, 40}, 95); got != 38.5 {
		t.Fatal(got)
	}
	if got := percentile([]float64{12}, 99); got != 12 {
		t.Fatal(got)
	}
}

func TestPingArgs(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     []string
	}{
		{"windows", []string{"-n", "1", "-w", "1500", "127.0.0.1"}},
		{"darwin", []string{"-n", "-c", "1", "-W", "1500", "127.0.0.1"}},
		{"linux", []string{"-n", "-c", "1", "-W", "2", "127.0.0.1"}},
	} {
		if got := pingArgs(tc.platform, "127.0.0.1", 1500*time.Millisecond); !reflect.DeepEqual(got, tc.want) {
			t.Fatal(got)
		}
	}
}
