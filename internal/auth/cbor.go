package auth

import (
	"errors"
	"math"
	"unicode/utf8"
)

var errCBOR = errors.New("invalid CBOR")

// cborItem reads one CBOR data item and returns what follows it. It covers the
// subset WebAuthn attestation and COSE keys use: integers, byte and text
// strings, arrays, maps keyed by an integer or text, tags (dropped) and
// false/true/null. Indefinite lengths and floats are refused, as CTAP2
// canonical encoding never produces them.
//
// Integers decode to int64, byte strings to []byte, text to string, arrays to
// []any and maps to map[any]any.
func cborItem(b []byte, depth int) (any, []byte, error) {
	if len(b) == 0 || depth > 8 {
		return nil, nil, errCBOR
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info <= 27:
		size := 1 << (info - 24)
		if len(b) < size {
			return nil, nil, errCBOR
		}
		for _, c := range b[:size] {
			n = n<<8 | uint64(c)
		}
		b = b[size:]
	default:
		return nil, nil, errCBOR
	}
	switch major {
	case 0, 1:
		if n > math.MaxInt64 {
			return nil, nil, errCBOR
		}
		if major == 1 {
			return -1 - int64(n), b, nil
		}
		return int64(n), b, nil
	case 2, 3:
		if n > uint64(len(b)) {
			return nil, nil, errCBOR
		}
		value := b[:n]
		if major == 2 {
			return append([]byte{}, value...), b[n:], nil
		}
		if !utf8.Valid(value) {
			return nil, nil, errCBOR
		}
		return string(value), b[n:], nil
	case 4:
		// Every item takes at least one byte, which bounds the allocation.
		if n > uint64(len(b)) {
			return nil, nil, errCBOR
		}
		out := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			v, rest, err := cborItem(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out, b = append(out, v), rest
		}
		return out, b, nil
	case 5:
		if n > uint64(len(b))/2 {
			return nil, nil, errCBOR
		}
		out := make(map[any]any, n)
		for i := uint64(0); i < n; i++ {
			k, rest, err := cborItem(b, depth+1)
			if err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, errCBOR
			}
			if _, dup := out[k]; dup {
				return nil, nil, errCBOR
			}
			v, rest, err := cborItem(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			out[k], b = v, rest
		}
		return out, b, nil
	case 6:
		return cborItem(b, depth+1)
	}
	switch info {
	case 20:
		return false, b, nil
	case 21:
		return true, b, nil
	case 22, 23:
		return nil, b, nil
	}
	return nil, nil, errCBOR
}
