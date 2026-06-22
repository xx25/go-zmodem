package zmodem

import (
	"encoding/binary"
	"fmt"
)

// sendSubpacket sends a data subpacket with the given end type.
// CRC scope: CRC covers data bytes AND the end-type byte itself.
func (s *Session) sendSubpacket(data []byte, endType byte) error {
	tw := s.tw

	if s.useCRC32 {
		// CRC-32: data + endType byte
		// Go's crc32.Update(0, table, data) handles init/final XOR internally,
		// producing the same result as crc32.ChecksumIEEE for incremental use.
		crc := crc32Update(0, data)
		crc = crc32Update(crc, []byte{endType})

		// Write escaped data
		if err := tw.writeEscaped(data); err != nil {
			return err
		}

		// ZDLE + endType
		if err := tw.writeByte(ZDLE); err != nil {
			return err
		}
		if err := tw.writeByte(endType); err != nil {
			return err
		}

		// CRC-32 escaped (little-endian)
		var crcBuf [4]byte
		binary.LittleEndian.PutUint32(crcBuf[:], crc)
		if err := tw.writeEscaped(crcBuf[:]); err != nil {
			return err
		}
	} else {
		// CRC-16: data + endType byte, then finalize
		crc := crc16Update(0, data)
		crc = crc16Update(crc, []byte{endType})
		crc = crc16Finalize(crc)

		// Write escaped data
		if err := tw.writeEscaped(data); err != nil {
			return err
		}

		// ZDLE + endType
		if err := tw.writeByte(ZDLE); err != nil {
			return err
		}
		if err := tw.writeByte(endType); err != nil {
			return err
		}

		// CRC-16 escaped (big-endian: high byte first)
		if err := tw.writeEscapedByte(byte(crc >> 8)); err != nil {
			return err
		}
		if err := tw.writeEscapedByte(byte(crc & 0xff)); err != nil {
			return err
		}
	}

	// Per lrzsz: send XON after ZCRCW subpackets
	if endType == ZCRCW {
		if err := tw.writeByte(XON); err != nil {
			return err
		}
	}

	return tw.Flush()
}

// recvSubpacket reads a data subpacket, returning data and end type.
// maxLen limits the data size to prevent resource exhaustion.
func (s *Session) recvSubpacket(maxLen int) ([]byte, byte, error) {
	var data []byte

	if s.useCRC32 {
		return s.recvSubpacketCRC32(maxLen)
	}
	return s.recvSubpacketCRC16(data, maxLen)
}

// detectMergedSubpacketCRC16 scans an already-CRC-valid subpacket for an
// EMBEDDED complete subpacket frame — the fingerprint of a lost ZDLE that
// silently merged two subpackets.
//
// When the wire drops the ZDLE that introduces a subpacket end-marker
// (ZDLE ZCRCx), the receiver does not see the boundary: it swallows the
// orphaned end-char + that subpacket's own 2 CRC bytes + the next subpacket's
// data as one oversized subpacket. Because a CRC-16 message followed by its own
// CRC has residue 0 (CRC-16/CCITT, init 0), the merged subpacket's CRC equals
// the SECOND subpacket's transmitted CRC, so the corrupt frame passes the outer
// CRC check and writes 3 stray bytes (end-char + CRC) at a valid offset.
//
// The tell is structural: at the swallowed boundary i, data[i] is a ZCRC
// end-char and CRC16(data[:i] + data[i]) matches the two bytes after it — i.e.
// data[:i] is itself a complete valid subpacket. A legitimate subpacket
// essentially never contains such an internal frame (~1/65536 per ZCRC-valued
// byte); a merge always does. Returns the split length (len of the embedded
// first subpacket) or -1.
func detectMergedSubpacketCRC16(data []byte) int {
	if len(data) < 4 {
		return -1
	}
	running := uint16(0) // crc16 of data[:i]
	for i := 1; i <= len(data)-3; i++ {
		running = crc16Update(running, data[i-1:i])
		c := data[i]
		if c < ZCRCE || c > ZCRCW {
			continue
		}
		want := crc16Finalize(crc16Update(running, data[i:i+1]))
		got := uint16(data[i+1])<<8 | uint16(data[i+2])
		if want == got {
			return i
		}
	}
	return -1
}

func (s *Session) recvSubpacketCRC16(data []byte, maxLen int) ([]byte, byte, error) {
	for {
		b, frameEnd, err := s.tr.zdlRead()
		if err != nil {
			return nil, 0, fmt.Errorf("subpacket read: %w", err)
		}

		if frameEnd != 0 {
			// Read 2-byte CRC (big-endian) via ZDLE decoding
			crcHi, fe, err := s.tr.zdlRead()
			if err != nil {
				return nil, 0, fmt.Errorf("subpacket CRC read: %w", err)
			}
			if fe != 0 {
				return nil, 0, fmt.Errorf("zmodem: unexpected frame end in subpacket CRC")
			}
			crcLo, fe, err := s.tr.zdlRead()
			if err != nil {
				return nil, 0, fmt.Errorf("subpacket CRC read: %w", err)
			}
			if fe != 0 {
				return nil, 0, fmt.Errorf("zmodem: unexpected frame end in subpacket CRC")
			}

			// Verify CRC-16: data + endType byte
			crc := crc16Update(0, data)
			crc = crc16Update(crc, []byte{frameEnd})
			crc = crc16Finalize(crc)

			recvCRC := uint16(crcHi)<<8 | uint16(crcLo)
			if crc != recvCRC {
				return nil, 0, fmt.Errorf("zmodem: subpacket CRC-16 error (computed=0x%04x, received=0x%04x)", crc, recvCRC)
			}

			return data, frameEnd, nil
		}

		if len(data) >= maxLen {
			return nil, 0, fmt.Errorf("zmodem: subpacket exceeds max length %d", maxLen)
		}
		data = append(data, b)
	}
}

func (s *Session) recvSubpacketCRC32(maxLen int) ([]byte, byte, error) {
	var data []byte

	for {
		b, frameEnd, err := s.tr.zdlRead()
		if err != nil {
			return nil, 0, fmt.Errorf("subpacket read: %w", err)
		}

		if frameEnd != 0 {
			// Read 4-byte CRC-32 (little-endian) via ZDLE decoding
			var crcBuf [4]byte
			for i := range crcBuf {
				cb, fe, err := s.tr.zdlRead()
				if err != nil {
					return nil, 0, fmt.Errorf("subpacket CRC32 read: %w", err)
				}
				if fe != 0 {
					return nil, 0, fmt.Errorf("zmodem: unexpected frame end in subpacket CRC32")
				}
				crcBuf[i] = cb
			}

			// Verify CRC-32: data + endType byte
			crc := crc32Update(0, data)
			crc = crc32Update(crc, []byte{frameEnd})

			recvCRC := binary.LittleEndian.Uint32(crcBuf[:])
			if crc != recvCRC {
				return nil, 0, fmt.Errorf("zmodem: subpacket CRC-32 error (computed=0x%08x, received=0x%08x)", crc, recvCRC)
			}

			return data, frameEnd, nil
		}

		if len(data) >= maxLen {
			return nil, 0, fmt.Errorf("zmodem: subpacket exceeds max length %d", maxLen)
		}
		data = append(data, b)
	}
}
