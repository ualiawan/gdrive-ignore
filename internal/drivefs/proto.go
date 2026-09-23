package drivefs

import "strings"

// parseSyncTargets reads the SyncTargets protobuf without a schema:
//
//	repeated Target targets = 1;
//	message Target { Account account = 1; string mount = 2; }
//	message Account { string id = 1; }
func parseSyncTargets(b []byte) []syncTarget {
	var out []syncTarget
	for _, f := range protoFields(b) {
		if f.num != 1 || f.bytes == nil {
			continue
		}
		var t syncTarget
		for _, g := range protoFields(f.bytes) {
			switch {
			case g.num == 1 && g.bytes != nil:
				for _, h := range protoFields(g.bytes) {
					if h.num == 1 && h.bytes != nil {
						t.account = string(h.bytes)
					}
				}
			case g.num == 2 && g.bytes != nil:
				t.mount = string(g.bytes)
			}
		}
		if len(t.mount) >= 2 && t.mount[1] == ':' {
			t.mount = strings.ToUpper(t.mount[:1]) + t.mount[1:]
			out = append(out, t)
		}
	}
	return out
}

type protoField struct {
	num   uint64
	bytes []byte // set for length-delimited fields
}

// protoFields splits a protobuf message into fields; it stops at the first
// malformed byte rather than failing.
func protoFields(b []byte) []protoField {
	var out []protoField
	for len(b) > 0 {
		tag, n := uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		f := protoField{num: tag >> 3}
		switch tag & 7 {
		case 0:
			_, n = uvarint(b)
			if n <= 0 {
				return out
			}
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return out
			}
			f.bytes = b[n : n+int(l)]
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	for i := 0; i < len(b) && i < 10; i++ {
		x |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}
