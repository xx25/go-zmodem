package zmodem

// Escape table values
const (
	escSend    = 0 // send directly
	escMust    = 1 // must escape (ZDLE + c^0x40)
	escIfAtCR  = 2 // escape only if preceded by '@' (CR protection)
)

// buildEscapeTable builds the ZDLE escape lookup table.
// escapeAll: escape all control characters (for hostile transports).
func buildEscapeTable(escapeAll bool) [256]byte {
	var table [256]byte

	// Always escape these regardless of mode:
	// ZDLE (0x18), DLE (0x10), XON (0x11), XOFF (0x13)
	// and their high-bit variants (0x90, 0x91, 0x93, 0x98)
	table[ZDLE] = escMust  // 0x18
	table[0x10] = escMust  // DLE
	table[XON] = escMust   // 0x11
	table[XOFF] = escMust  // 0x13
	table[0x90] = escMust  // DLE | 0x80
	table[0x91] = escMust  // XON | 0x80
	table[0x93] = escMust  // XOFF | 0x80
	table[0x98] = escMust  // ZDLE | 0x80

	// CR (0x0d) and 0x8d: escape if preceded by '@' (Telenet CR-@-CR protection)
	table[0x0d] = escIfAtCR
	table[0x8d] = escIfAtCR

	if escapeAll {
		// Escape all characters with bits 5+6 both zero (control chars 0x00-0x1F)
		// and their high-bit variants (0x80-0x9F).
		// Note: 0x7F (DEL) and 0xFF are NOT escaped — they have bits 5+6 set
		// and ZDLE+XOR encoding cannot represent them. lrzsz matches this behavior.
		for i := 0; i < 32; i++ {
			if table[i] == escSend {
				table[i] = escMust
			}
			if table[i|0x80] == escSend {
				table[i|0x80] = escMust
			}
		}
	}

	return table
}

// escapeRequired returns true if byte b must be escaped given the table and lastSent byte.
func escapeRequired(table *[256]byte, b byte, lastSent byte) bool {
	switch table[b] {
	case escMust:
		return true
	case escIfAtCR:
		return lastSent == '@' || lastSent == 0xc0
	default:
		return false
	}
}

// escapeByte returns the escaped form: ZDLE followed by (b ^ 0x40).
func escapeByte(b byte) (byte, byte) {
	return ZDLE, b ^ 0x40
}

// unescapeByte reverses the escape: ZDLE-encoded byte c → original byte.
// The caller has already consumed the ZDLE prefix.
func unescapeByte(c byte) byte {
	return c ^ 0x40
}
