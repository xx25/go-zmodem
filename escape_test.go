package zmodem

import (
	"testing"
)

func TestBuildEscapeTable(t *testing.T) {
	table := buildEscapeTable(EscapeStandard)

	// Must-escape characters
	mustEscape := []byte{ZDLE, 0x10, XON, XOFF, 0x90, 0x91, 0x93, 0x98}
	for _, b := range mustEscape {
		if table[b] != escMust {
			t.Errorf("byte 0x%02x should be escMust, got %d", b, table[b])
		}
	}

	// CR should be conditional
	if table[0x0d] != escIfAtCR {
		t.Errorf("CR (0x0d) should be escIfAtCR, got %d", table[0x0d])
	}

	// Normal byte should pass through
	if table['A'] != escSend {
		t.Errorf("'A' should be escSend, got %d", table['A'])
	}
}

func TestBuildEscapeTableAll(t *testing.T) {
	table := buildEscapeTable(EscapeAll)

	// All control chars (except CR which is escIfAtCR) should be escMust
	for i := 0; i < 32; i++ {
		if i == 0x0d { // CR
			continue
		}
		if table[i] != escMust {
			t.Errorf("escapeAll: byte 0x%02x should be escMust, got %d", i, table[i])
		}
	}

	// High-bit variants too
	for i := 0x80; i < 0xa0; i++ {
		if i == 0x8d { // CR | 0x80
			continue
		}
		if table[i] != escMust {
			t.Errorf("escapeAll: byte 0x%02x should be escMust, got %d", i, table[i])
		}
	}
}

func TestBuildEscapeTableMinimal(t *testing.T) {
	table := buildEscapeTable(EscapeMinimal)

	// Only ZDLE should be escMust
	if table[ZDLE] != escMust {
		t.Errorf("ZDLE (0x18) should be escMust, got %d", table[ZDLE])
	}

	// Everything else should be escSend (no escaping)
	for i := 0; i < 256; i++ {
		if byte(i) == ZDLE {
			continue
		}
		if table[i] != escSend {
			t.Errorf("byte 0x%02x should be escSend in minimal mode, got %d", i, table[i])
		}
	}
}

func TestEscapeRequired(t *testing.T) {
	table := buildEscapeTable(EscapeStandard)

	// ZDLE always needs escape
	if !escapeRequired(&table, ZDLE, 0) {
		t.Error("ZDLE should require escape")
	}

	// CR after '@' needs escape
	if !escapeRequired(&table, 0x0d, '@') {
		t.Error("CR after '@' should require escape")
	}

	// CR after 'A' doesn't need escape
	if escapeRequired(&table, 0x0d, 'A') {
		t.Error("CR after 'A' should not require escape")
	}

	// CR after 0xc0 needs escape
	if !escapeRequired(&table, 0x0d, 0xc0) {
		t.Error("CR after 0xc0 should require escape")
	}

	// Normal byte never needs escape
	if escapeRequired(&table, 'Z', 0) {
		t.Error("'Z' should not require escape")
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	// Test that escape + unescape is identity
	for i := 0; i < 256; i++ {
		b := byte(i)
		_, escaped := escapeByte(b)
		recovered := unescapeByte(escaped)
		if recovered != b {
			t.Errorf("round-trip failed for 0x%02x: escaped=0x%02x, recovered=0x%02x", b, escaped, recovered)
		}
	}
}
