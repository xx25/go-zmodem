package zmodem

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestHexHeaderRoundTrip(t *testing.T) {
	// Create a pipe-like transport using bytes.Buffer
	var buf bytes.Buffer

	tw := newTransportWriter(&buf, EscapeStandard)
	tr := newTransportReader(&buf, 1200, 0, true, slog.Default())

	s := &Session{
		tw:     tw,
		tr:     tr,
		logger: slog.Default(),
	}

	// Test various frame types
	tests := []struct {
		name     string
		hdr      Header
	}{
		{"ZRQINIT", makeHeader(ZRQINIT)},
		{"ZRINIT", makePosHeader(ZRINIT, 0)},
		{"ZACK", makePosHeader(ZACK, 12345)},
		{"ZRPOS", makePosHeader(ZRPOS, 0x12345678)},
		{"ZEOF", makePosHeader(ZEOF, 1000)},
		{"ZFIN", makeHeader(ZFIN)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()

			if err := s.sendHexHeader(tc.hdr); err != nil {
				t.Fatalf("sendHexHeader: %v", err)
			}

			got, err := s.recvHeader()
			if err != nil {
				t.Fatalf("recvHeader: %v", err)
			}

			if got.Type != tc.hdr.Type {
				t.Errorf("type = 0x%02x, want 0x%02x", got.Type, tc.hdr.Type)
			}
			if got.Data != tc.hdr.Data {
				t.Errorf("data = %v, want %v", got.Data, tc.hdr.Data)
			}
			if got.Encoding != ZHEX {
				t.Errorf("encoding = 0x%02x, want ZHEX (0x%02x)", got.Encoding, ZHEX)
			}
		})
	}
}

func TestBinHeaderRoundTripCRC16(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: false,
	}

	hdr := makePosHeader(ZDATA, 0xABCD1234)

	if err := s.sendBinHeader(hdr); err != nil {
		t.Fatalf("sendBinHeader: %v", err)
	}

	got, err := s.recvHeader()
	if err != nil {
		t.Fatalf("recvHeader: %v", err)
	}

	if got.Type != hdr.Type {
		t.Errorf("type = 0x%02x, want 0x%02x", got.Type, hdr.Type)
	}
	if got.Data != hdr.Data {
		t.Errorf("data = %v, want %v", got.Data, hdr.Data)
	}
	if got.Encoding != ZBIN {
		t.Errorf("encoding = 0x%02x, want ZBIN", got.Encoding)
	}
}

func TestBinHeaderRoundTripCRC32(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: true,
	}

	hdr := makePosHeader(ZFILE, 0)

	if err := s.sendBinHeader(hdr); err != nil {
		t.Fatalf("sendBinHeader: %v", err)
	}

	got, err := s.recvHeader()
	if err != nil {
		t.Fatalf("recvHeader: %v", err)
	}

	if got.Type != hdr.Type {
		t.Errorf("type = 0x%02x, want 0x%02x", got.Type, hdr.Type)
	}
	if got.Data != hdr.Data {
		t.Errorf("data = %v, want %v", got.Data, hdr.Data)
	}
	if got.Encoding != ZBIN32 {
		t.Errorf("encoding = 0x%02x, want ZBIN32", got.Encoding)
	}
}

func TestHeaderPosition(t *testing.T) {
	hdr := Header{}
	hdr.SetPosition(0x12345678)

	if hdr.Position() != 0x12345678 {
		t.Errorf("Position() = 0x%x, want 0x12345678", hdr.Position())
	}

	// Test that Data bytes are little-endian
	if hdr.Data[0] != 0x78 || hdr.Data[1] != 0x56 || hdr.Data[2] != 0x34 || hdr.Data[3] != 0x12 {
		t.Errorf("Data = %v, want [0x78 0x56 0x34 0x12]", hdr.Data)
	}
}

func TestHeaderFlags(t *testing.T) {
	hdr := Header{}
	hdr.SetZF0(0xAA)
	hdr.SetZF1(0xBB)
	hdr.SetZF2(0xCC)
	hdr.SetZF3(0xDD)

	if hdr.ZF0() != 0xAA {
		t.Errorf("ZF0 = 0x%02x, want 0xAA", hdr.ZF0())
	}
	if hdr.ZF1() != 0xBB {
		t.Errorf("ZF1 = 0x%02x, want 0xBB", hdr.ZF1())
	}
	if hdr.ZF2() != 0xCC {
		t.Errorf("ZF2 = 0x%02x, want 0xCC", hdr.ZF2())
	}
	if hdr.ZF3() != 0xDD {
		t.Errorf("ZF3 = 0x%02x, want 0xDD", hdr.ZF3())
	}

	// Verify that flags and position use opposite byte orders
	// ZF0 is Data[3], ZF3 is Data[0]
	if hdr.Data[3] != 0xAA {
		t.Errorf("Data[3] (ZF0) = 0x%02x, want 0xAA", hdr.Data[3])
	}
	if hdr.Data[0] != 0xDD {
		t.Errorf("Data[0] (ZF3) = 0x%02x, want 0xDD", hdr.Data[0])
	}
}

func TestHexHeaderLowercaseDigits(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:     newTransportWriter(&buf, EscapeStandard),
		tr:     newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger: slog.Default(),
	}

	hdr := makePosHeader(ZACK, 0xABCDEF01)
	if err := s.sendHexHeader(hdr); err != nil {
		t.Fatalf("sendHexHeader: %v", err)
	}

	// Check that the output only contains lowercase hex digits
	out := buf.Bytes()
	// Skip ZPAD ZPAD ZDLE ZHEX prefix (4 bytes)
	hexPart := out[4:]
	for i, b := range hexPart {
		if b >= 'A' && b <= 'F' {
			t.Errorf("uppercase hex digit at offset %d: 0x%02x (%c)", i, b, b)
		}
	}
}
