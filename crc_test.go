package zmodem

import (
	"encoding/binary"
	"testing"
)

func TestCRC16Calc(t *testing.T) {
	// Known test vector: "123456789" should give CRC-16/XMODEM = 0x31C3
	// But with ZMODEM finalization (two zero bytes), the result differs.
	data := []byte("123456789")
	crc := crc16Calc(data)

	// Verify round-trip: data + CRC should verify to 0
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], crc)
	all := append(data, buf[:]...)
	if !crc16Verify(all) {
		t.Errorf("CRC-16 verify failed for test vector, crc=0x%04x", crc)
	}
}

func TestCRC16EmptyData(t *testing.T) {
	// CRC-16/XMODEM of empty data with finalization is 0
	// (init=0, no data, feed [0,0] through CRC with init 0 → 0)
	crc := crc16Calc([]byte{})
	if crc != 0 {
		t.Errorf("CRC-16 of empty data with finalization should be 0, got 0x%04x", crc)
	}
}

func TestCRC16Incremental(t *testing.T) {
	data := []byte("Hello, ZMODEM!")
	expected := crc16Calc(data)

	// Build incrementally
	crc := crc16Update(0, data[:5])
	crc = crc16Update(crc, data[5:])
	crc = crc16Finalize(crc)

	if crc != expected {
		t.Errorf("incremental CRC-16 mismatch: got 0x%04x, want 0x%04x", crc, expected)
	}
}

func TestCRC32Calc(t *testing.T) {
	data := []byte("123456789")
	crc := crc32Calc(data)

	// Known CRC-32/IEEE of "123456789" = 0xCBF43926
	if crc != 0xCBF43926 {
		t.Errorf("CRC-32 of '123456789' = 0x%08x, want 0xCBF43926", crc)
	}
}

func TestCRC32Verify(t *testing.T) {
	data := []byte("Hello, ZMODEM!")
	crc := crc32Calc(data)

	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], crc)
	all := append(data, buf[:]...)
	if !crc32Verify(all) {
		t.Errorf("CRC-32 verify failed, crc=0x%08x", crc)
	}
}

func TestCRC32Incremental(t *testing.T) {
	data := []byte("Hello, ZMODEM!")
	expected := crc32Calc(data)

	crc := crc32Update(0, data[:5])
	crc = crc32Update(crc, data[5:])

	if crc != expected {
		t.Errorf("incremental CRC-32 mismatch: got 0x%08x, want 0x%08x", crc, expected)
	}
}
