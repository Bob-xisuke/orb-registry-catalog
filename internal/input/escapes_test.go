package input

// jsonEscape builds the six-character literal text backslash-u followed by
// hex, e.g. jsonEscape("0072") is the JSON escape for 'r'. It is assembled
// from the backslash byte so the escape sequence never appears literally in
// this source.
func jsonEscape(hex string) string { return string([]byte{0x5c}) + "u" + hex }

// Literal JSON unicode escapes for the recognized key names:
// r='r', d='d', t='t', v='v' (signature_verified), b='b' (size_bytes).
var (
	escR = jsonEscape("0072")
	escD = jsonEscape("0064")
	escT = jsonEscape("0074")
	escV = jsonEscape("0076")
	escB = jsonEscape("0062")
)
