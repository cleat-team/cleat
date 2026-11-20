package engine

import "context"

// DefaultMaxHostCallResponseBytes is the default ceiling on one host call's response, for every guest language (2 MiB, cleat#4132; the owner's
// choice, matching the 2 MiB limit on requests, cleat#4130).
const DefaultMaxHostCallResponseBytes = 2097152

// maxPackableResponseBytes is the largest response a durable-call result word can carry: packDurableCallResult gives the length 24 bits.
const maxPackableResponseBytes = 1<<24 - 1

// MaxHostCallResponseBytes bounds one host call's response. A response over it is not truncated silently: the call fails with code 7
// (OutputTruncated) carrying the prefix that fit, as before. Set from --wasm-max-response-bytes before any Runtime is created.
//
// It is NOT OutBufSize. That sizes the entry-point input and output regions in a module's linear memory (and so bounds a workflow's input and
// result), and raising it grows every instance's memory; this bounds what a host call may hand back and costs nothing until a response is that
// large. The two were conflated in the first design of cleat#4132 and are tracked in their own issue.
var MaxHostCallResponseBytes uint32 = DefaultMaxHostCallResponseBytes

// hostCallResponseLimit is MaxHostCallResponseBytes clamped to what a result word can carry, so an operator who sets the flag above 16 MiB gets
// 16 MiB - 1 rather than a length that wraps in the packed word.
func hostCallResponseLimit() uint32 {
	if MaxHostCallResponseBytes > maxPackableResponseBytes {
		return maxPackableResponseBytes
	}
	return MaxHostCallResponseBytes
}

// responseBufKey carries a *responseBuf in a context.
type responseBufKey struct{}

// responseBuf is the buffer a component dispatcher hands a host call to write its response into. The component path used to allocate
// make([]byte, 65536) before the call, so no response could exceed 64 KiB however large the host's own was (cleat#4132). The host does not
// know the size before the call, so the buffer is not allocated up front: it starts empty and writeResult sizes it to exactly the response (up
// to the ceiling), so a 200-byte reply costs 200 bytes and a 1 MiB reply costs 1 MiB.
type responseBuf struct{ b []byte }

// newResponseBuf returns a buffer and a context that makes the host call write into it.
func newResponseBuf(ctx context.Context) (*responseBuf, context.Context) {
	rb := &responseBuf{}
	return rb, context.WithValue(contextWithRawMemBuf(ctx, []byte{}), responseBufKey{}, rb)
}

// bytes returns what the host call wrote. Empty when it wrote nothing.
func (r *responseBuf) bytes() []byte { return r.b }

// ensure grows the buffer to at least n bytes, keeping what is there.
func (r *responseBuf) ensure(n uint64) []byte {
	if uint64(len(r.b)) < n {
		grown := make([]byte, n)
		copy(grown, r.b)
		r.b = grown
	}
	return r.b
}

// responseBufFrom returns the growable buffer carried by ctx, if any.
func responseBufFrom(ctx context.Context) (*responseBuf, bool) {
	rb, ok := ctx.Value(responseBufKey{}).(*responseBuf)
	return rb, ok
}
