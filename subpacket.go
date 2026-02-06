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
