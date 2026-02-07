package zmodem

import (
	"encoding/binary"
	"fmt"
)

// Header represents a ZMODEM frame header.
type Header struct {
	Encoding byte    // ZBIN, ZHEX, or ZBIN32
	Type     byte    // Frame type (ZRQINIT, ZRINIT, etc.)
	Data     [4]byte // 4 bytes of position/flags
}

// Position returns header data as a 32-bit file offset (little-endian).
func (h *Header) Position() int64 {
	return int64(binary.LittleEndian.Uint32(h.Data[:]))
}

// SetPosition sets header data from a file offset (little-endian).
func (h *Header) SetPosition(pos int64) {
	binary.LittleEndian.PutUint32(h.Data[:], uint32(pos))
}

// ZF0-ZF3 flag accessors.
// IMPORTANT: flags and position use OPPOSITE byte orders in the same 4 bytes!
// Flags: TYPE ZF3 ZF2 ZF1 ZF0 (ZF0 = Data[3])
// Position: TYPE P0 P1 P2 P3 (P0 = Data[0])
func (h *Header) ZF0() byte     { return h.Data[3] }
func (h *Header) ZF1() byte     { return h.Data[2] }
func (h *Header) ZF2() byte     { return h.Data[1] }
func (h *Header) ZF3() byte     { return h.Data[0] }
func (h *Header) SetZF0(v byte) { h.Data[3] = v }
func (h *Header) SetZF1(v byte) { h.Data[2] = v }
func (h *Header) SetZF2(v byte) { h.Data[1] = v }
func (h *Header) SetZF3(v byte) { h.Data[0] = v }

// String returns a human-readable representation.
func (h Header) String() string {
	return fmt.Sprintf("%s[%02x %02x %02x %02x]",
		frameTypeName(h.Type), h.Data[0], h.Data[1], h.Data[2], h.Data[3])
}

// makeHeader creates a header with the given type and zero data.
func makeHeader(frameType byte) Header {
	return Header{Type: frameType}
}

// makePosHeader creates a header with a position value.
func makePosHeader(frameType byte, pos int64) Header {
	h := Header{Type: frameType}
	h.SetPosition(pos)
	return h
}

// sendHexHeader sends a HEX-encoded frame header.
// Format: ZPAD ZPAD ZDLE ZHEX <type> <data[0..3]> <crc16> CR LF [XON]
// All values as 2 lowercase hex digits. Always CRC-16.
func (s *Session) sendHexHeader(hdr Header) error {
	s.logger.Debug("send hex header", "type", frameTypeName(hdr.Type), "data", fmt.Sprintf("%v", hdr.Data))

	tw := s.tw
	// Header prefix
	if err := tw.writeRaw([]byte{ZPAD, ZPAD, ZDLE, ZHEX}); err != nil {
		return err
	}

	// Build the 5-byte payload: type + data[0..3]
	var payload [5]byte
	payload[0] = hdr.Type
	copy(payload[1:], hdr.Data[:])

	// CRC-16 of the payload (with 2-zero-byte finalization)
	crc := crc16Calc(payload[:])

	// Write type + data as hex
	for _, b := range payload {
		if err := tw.writeHex(b); err != nil {
			return err
		}
	}

	// Write CRC as hex (big-endian: high byte first, then low byte)
	if err := tw.writeHex(byte(crc >> 8)); err != nil {
		return err
	}
	if err := tw.writeHex(byte(crc & 0xff)); err != nil {
		return err
	}

	// CR LF terminator
	if err := tw.writeByte(0x0d); err != nil {
		return err
	}
	if err := tw.writeByte(0x0a); err != nil {
		return err
	}

	// Append XON except for ZACK and ZFIN
	if hdr.Type != ZACK && hdr.Type != ZFIN {
		if err := tw.writeByte(XON); err != nil {
			return err
		}
	}

	return tw.Flush()
}

// sendBinHeader sends a binary frame header (ZBIN or ZBIN32 depending on session CRC mode).
// Format: ZPAD ZDLE <enc> <type-escaped> <data[0..3]-escaped> <crc-escaped>
func (s *Session) sendBinHeader(hdr Header) error {
	s.logger.Debug("send bin header", "type", frameTypeName(hdr.Type),
		"data", fmt.Sprintf("%v", hdr.Data), "crc32", s.useCRC32)

	tw := s.tw

	var enc byte
	if s.useCRC32 {
		enc = ZBIN32
	} else {
		enc = ZBIN
	}

	// Header prefix (not escaped)
	if err := tw.writeRaw([]byte{ZPAD, ZDLE, enc}); err != nil {
		return err
	}

	// Build the 5-byte payload: type + data[0..3]
	var payload [5]byte
	payload[0] = hdr.Type
	copy(payload[1:], hdr.Data[:])

	if s.useCRC32 {
		crc := crc32Calc(payload[:])
		// Write payload escaped
		if err := tw.writeEscaped(payload[:]); err != nil {
			return err
		}
		// Write CRC-32 escaped (little-endian)
		var crcBuf [4]byte
		binary.LittleEndian.PutUint32(crcBuf[:], crc)
		if err := tw.writeEscaped(crcBuf[:]); err != nil {
			return err
		}
	} else {
		crc := crc16Calc(payload[:])
		// Write payload escaped
		if err := tw.writeEscaped(payload[:]); err != nil {
			return err
		}
		// Write CRC-16 escaped (big-endian: high byte first)
		if err := tw.writeEscapedByte(byte(crc >> 8)); err != nil {
			return err
		}
		if err := tw.writeEscapedByte(byte(crc & 0xff)); err != nil {
			return err
		}
	}

	return tw.Flush()
}

// sendBinHeaderWithZnulls sends Znulls null bytes then a binary header.
// Used before ZDATA headers for modem turnaround.
func (s *Session) sendBinHeaderWithZnulls(hdr Header) error {
	if s.cfg.Znulls > 0 {
		nulls := make([]byte, s.cfg.Znulls)
		if err := s.tw.writeRaw(nulls); err != nil {
			return err
		}
	}
	return s.sendBinHeader(hdr)
}

// recvHeader receives and decodes a frame header.
// Auto-detects HEX/ZBIN/ZBIN32 encoding.
func (s *Session) recvHeader() (Header, error) {
	enc, err := s.tr.scanForPad()
	if err != nil {
		return Header{}, err
	}

	var hdr Header
	hdr.Encoding = enc

	switch enc {
	case ZHEX:
		hdr, err = s.recvHexHeader()
	case ZBIN:
		hdr, err = s.recvBinHeader(false)
	case ZBIN32:
		hdr, err = s.recvBinHeader(true)
	default:
		return Header{}, fmt.Errorf("%w: 0x%02x", errUnsupportedEnc, enc)
	}

	if err != nil {
		return Header{}, err
	}

	s.tr.resetGarbage()
	s.logger.Debug("recv header", "type", frameTypeName(hdr.Type),
		"data", fmt.Sprintf("%v", hdr.Data), "encoding", fmt.Sprintf("0x%02x", enc))

	// Warn about HyperTerminal extended types
	if hdr.Type > ZSTDERR && hdr.Type <= maxFrameType {
		s.logger.Warn("received HyperTerminal extended frame type",
			"type", frameTypeName(hdr.Type), "code", hdr.Type)
	}

	return hdr, nil
}

// recvHexHeader reads a HEX-encoded header (after ZPAD ZPAD ZDLE ZHEX consumed).
func (s *Session) recvHexHeader() (Header, error) {
	var hdr Header
	hdr.Encoding = ZHEX

	// Read type + 4 data bytes + 2 CRC bytes = 7 hex-encoded bytes
	var raw [7]byte
	for i := range raw {
		b, err := s.tr.readHex()
		if err != nil {
			return Header{}, fmt.Errorf("hex header read: %w", err)
		}
		raw[i] = b
	}

	hdr.Type = raw[0]
	copy(hdr.Data[:], raw[1:5])

	// Verify CRC-16 (includes finalization)
	if !crc16Verify(raw[:]) {
		return Header{}, fmt.Errorf("zmodem: hex header CRC error for %s", frameTypeName(hdr.Type))
	}

	// Read CR LF terminator (strip parity bits)
	cr, err := s.tr.readByte()
	if err != nil {
		return Header{}, err
	}
	if cr&0x7f != 0x0d {
		// Some implementations may send LF only
		if cr&0x7f == 0x0a {
			return hdr, nil
		}
		return Header{}, fmt.Errorf("zmodem: expected CR after hex header, got 0x%02x", cr)
	}

	lf, err := s.tr.readByte()
	if err != nil {
		return Header{}, err
	}
	if lf&0x7f != 0x0a {
		return Header{}, fmt.Errorf("zmodem: expected LF after hex header CR, got 0x%02x", lf)
	}

	// XON may follow (except for ZACK/ZFIN) — consume if present.
	// Only attempt if data is already buffered to avoid blocking.
	if hdr.Type != ZACK && hdr.Type != ZFIN && s.tr.r.Buffered() > 0 {
		peek, err := s.tr.r.Peek(1)
		if err == nil && len(peek) > 0 && (peek[0]&0x7f) == XON {
			_, _ = s.tr.readByte() // consume XON
		}
	}

	return hdr, nil
}

// recvBinHeader reads a binary-encoded header (after ZPAD ZDLE ZBIN/ZBIN32 consumed).
func (s *Session) recvBinHeader(crc32mode bool) (Header, error) {
	var hdr Header
	if crc32mode {
		hdr.Encoding = ZBIN32
	} else {
		hdr.Encoding = ZBIN
	}

	// Read type + 4 data bytes via ZDLE decoding
	var payload [5]byte
	for i := range payload {
		b, frameEnd, err := s.tr.zdlRead()
		if err != nil {
			return Header{}, fmt.Errorf("bin header read: %w", err)
		}
		if frameEnd != 0 {
			return Header{}, fmt.Errorf("zmodem: unexpected frame end in header")
		}
		payload[i] = b
	}

	hdr.Type = payload[0]
	copy(hdr.Data[:], payload[1:5])

	if crc32mode {
		// Read 4-byte CRC-32
		var crcBuf [4]byte
		for i := range crcBuf {
			b, frameEnd, err := s.tr.zdlRead()
			if err != nil {
				return Header{}, fmt.Errorf("bin32 header CRC read: %w", err)
			}
			if frameEnd != 0 {
				return Header{}, fmt.Errorf("zmodem: unexpected frame end in CRC")
			}
			crcBuf[i] = b
		}
		// Verify: payload + CRC should verify
		var all [9]byte
		copy(all[:5], payload[:])
		copy(all[5:], crcBuf[:])
		if !crc32Verify(all[:]) {
			return Header{}, fmt.Errorf("zmodem: bin32 header CRC error for %s", frameTypeName(hdr.Type))
		}
	} else {
		// Read 2-byte CRC-16 (big-endian)
		var crcBuf [2]byte
		for i := range crcBuf {
			b, frameEnd, err := s.tr.zdlRead()
			if err != nil {
				return Header{}, fmt.Errorf("bin header CRC read: %w", err)
			}
			if frameEnd != 0 {
				return Header{}, fmt.Errorf("zmodem: unexpected frame end in CRC")
			}
			crcBuf[i] = b
		}
		// Verify: payload + CRC bytes
		var all [7]byte
		copy(all[:5], payload[:])
		all[5] = crcBuf[0]
		all[6] = crcBuf[1]
		if !crc16Verify(all[:]) {
			return Header{}, fmt.Errorf("zmodem: bin header CRC error for %s", frameTypeName(hdr.Type))
		}
	}

	return hdr, nil
}
