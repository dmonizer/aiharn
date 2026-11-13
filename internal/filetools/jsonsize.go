package filetools

import "unicode/utf8"

// jsonSizeOf returns an upper bound, in bytes, of the JSON encoding of s
// (including the surrounding quotes). It exists so a read can refuse a range
// whose encoded result would exceed the model-visible budget before it mints a
// receipt for content the model could not see in full.
func jsonSizeOf(s string) int {
	size := 2 // surrounding quotes
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == '"' || b == '\\':
			size += 2
		case b < 0x20:
			size += 6 // \u00XX
		case b < utf8.RuneSelf:
			size++
		default:
			r, width := utf8.DecodeRuneInString(s[i:])
			if r == '\u2028' || r == '\u2029' {
				size += 6
			} else {
				size += width
			}
			i += width - 1
		}
	}
	return size
}

// envelopeBound returns an upper bound on the encoded size of a read result with
// the given content and filename. The constant covers the field names, numeric
// coordinates, booleans, punctuation, and the receipt id.
func envelopeBound(filename, content string) int {
	const metadataBound = 256
	return jsonSizeOf(filename) + jsonSizeOf(content) + metadataBound
}
