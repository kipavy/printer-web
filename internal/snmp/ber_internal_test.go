package snmp

import (
	"bytes"
	"testing"
)

func TestIntBytesBoundaries(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x00, 0x80}},
		{129, []byte{0x00, 0x81}},
		{-128, []byte{0x80}},
		{-129, []byte{0xff, 0x7f}},
		{255, []byte{0x00, 0xff}},
		{256, []byte{0x01, 0x00}},
		{-32768, []byte{0x80, 0x00}},
		{-32769, []byte{0xff, 0x7f, 0xff}},
		{4294967295, []byte{0x00, 0xff, 0xff, 0xff, 0xff}},
	} {
		got := intBytes(c.n)
		if !bytes.Equal(got, c.want) {
			t.Errorf("intBytes(%d) = % x, want % x", c.n, got, c.want)
		}
		if back, err := parseInt(got); err != nil || back != c.n {
			t.Errorf("parseInt(% x) = %d, %v", got, back, err)
		}
	}
}

func TestOIDBytesLargeFirstArcs(t *testing.T) {
	// 2.4294967295: 80 + 4294967295 does not fit in 32 bits.
	b, err := oidBytes("2.4294967295.1")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := parseOIDBytes(b); err != nil || s != "2.4294967295.1" {
		t.Fatalf("got %q %v", s, err)
	}
}
