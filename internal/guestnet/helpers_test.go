package guestnet

import "testing"

// concat joins message buffers as the kernel would in a multipart dump.
func concat(bufs ...[]byte) []byte {
	var out []byte
	for _, b := range bufs {
		out = append(out, b...)
	}
	return out
}

// buildDone constructs a bare NLMSG_DONE terminator message.
func buildDone(seq uint32) []byte {
	b := make([]byte, nlMsgHdrLen)
	nativeEndian.PutUint32(b[0:4], nlMsgHdrLen)
	nativeEndian.PutUint16(b[4:6], nlMsgDone)
	nativeEndian.PutUint32(b[8:12], seq)
	return b
}

// buildError constructs an NLMSG_ERROR message carrying errno (0 means ACK).
func buildError(seq uint32, errno int32) []byte {
	b := make([]byte, nlMsgHdrLen+4)
	nativeEndian.PutUint32(b[0:4], uint32(len(b)))
	nativeEndian.PutUint16(b[4:6], nlMsgError)
	nativeEndian.PutUint32(b[8:12], seq)
	nativeEndian.PutUint32(b[nlMsgHdrLen:], uint32(errno))
	return b
}

// splitMsg decodes the leading nlmsghdr and returns it with the message body
// (everything after the header).
func splitMsg(t *testing.T, msg []byte) (nlMsghdr, []byte) {
	t.Helper()
	if len(msg) < nlMsgHdrLen {
		t.Fatalf("message too short: %d bytes", len(msg))
	}
	h := nlMsghdr{
		Len:   nativeEndian.Uint32(msg[0:4]),
		Type:  nativeEndian.Uint16(msg[4:6]),
		Flags: nativeEndian.Uint16(msg[6:8]),
		Seq:   nativeEndian.Uint32(msg[8:12]),
		Pid:   nativeEndian.Uint32(msg[12:16]),
	}
	return h, msg[nlMsgHdrLen:]
}
