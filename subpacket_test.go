package zmodem

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestSubpacketRoundTripCRC16(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: false,
	}

	testData := []byte("Hello, ZMODEM protocol!")

	endTypes := []byte{ZCRCE, ZCRCG, ZCRCQ, ZCRCW}
	for _, et := range endTypes {
		t.Run(frameEndName(et), func(t *testing.T) {
			buf.Reset()

			if err := s.sendSubpacket(testData, et); err != nil {
				t.Fatalf("sendSubpacket: %v", err)
			}

			got, gotEnd, err := s.recvSubpacket(1024)
			if err != nil {
				t.Fatalf("recvSubpacket: %v", err)
			}

			if !bytes.Equal(got, testData) {
				t.Errorf("data mismatch: got %q, want %q", got, testData)
			}
			if gotEnd != et {
				t.Errorf("endType = 0x%02x, want 0x%02x", gotEnd, et)
			}
		})
	}
}

func TestSubpacketRoundTripCRC32(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: true,
	}

	testData := []byte("CRC-32 subpacket test data with special bytes: \x00\x10\x11\x13\x18\x7f\xff")

	if err := s.sendSubpacket(testData, ZCRCG); err != nil {
		t.Fatalf("sendSubpacket: %v", err)
	}

	got, gotEnd, err := s.recvSubpacket(1024)
	if err != nil {
		t.Fatalf("recvSubpacket: %v", err)
	}

	if !bytes.Equal(got, testData) {
		t.Errorf("data mismatch: got len=%d, want len=%d", len(got), len(testData))
	}
	if gotEnd != ZCRCG {
		t.Errorf("endType = 0x%02x, want ZCRCG", gotEnd)
	}
}

func TestSubpacketEmptyData(t *testing.T) {
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: false,
	}

	if err := s.sendSubpacket([]byte{}, ZCRCE); err != nil {
		t.Fatalf("sendSubpacket: %v", err)
	}

	got, gotEnd, err := s.recvSubpacket(1024)
	if err != nil {
		t.Fatalf("recvSubpacket: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(got))
	}
	if gotEnd != ZCRCE {
		t.Errorf("endType = 0x%02x, want ZCRCE", gotEnd)
	}
}

func TestSubpacketAllZDLEBytes(t *testing.T) {
	// Worst case: data full of bytes that need escaping
	var buf bytes.Buffer

	s := &Session{
		tw:       newTransportWriter(&buf, EscapeStandard),
		tr:       newTransportReader(&buf, 1200, 0, true, slog.Default()),
		logger:   slog.Default(),
		useCRC32: false,
	}

	// All ZDLE bytes
	testData := make([]byte, 64)
	for i := range testData {
		testData[i] = ZDLE
	}

	if err := s.sendSubpacket(testData, ZCRCW); err != nil {
		t.Fatalf("sendSubpacket: %v", err)
	}

	got, gotEnd, err := s.recvSubpacket(1024)
	if err != nil {
		t.Fatalf("recvSubpacket: %v", err)
	}

	if !bytes.Equal(got, testData) {
		t.Errorf("data mismatch for all-ZDLE test")
	}
	if gotEnd != ZCRCW {
		t.Errorf("endType = 0x%02x, want ZCRCW", gotEnd)
	}
}

func frameEndName(et byte) string {
	switch et {
	case ZCRCE:
		return "ZCRCE"
	case ZCRCG:
		return "ZCRCG"
	case ZCRCQ:
		return "ZCRCQ"
	case ZCRCW:
		return "ZCRCW"
	default:
		return "UNKNOWN"
	}
}
