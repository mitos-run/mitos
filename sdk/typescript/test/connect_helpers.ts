// Test helpers for faking a Connect sandbox.v1.Sandbox server and asserting the
// wire the SDK's ConnectClient produces. These mirror the on-the-wire framing in
// src/connect.ts: a 5-byte envelope prefix (1 flag byte + 4-byte big-endian
// length) per message, application/connect+json for streams, application/json
// for unary.

const FLAG_END_STREAM = 0b00000010;

/** base64-encode a UTF-8 string (a proto-JSON bytes field). */
export function b64(s: string): string {
  return Buffer.from(s, "utf-8").toString("base64");
}

/** Encode one message payload in the Connect 5-byte envelope prefix. */
export function encodeFrame(payload: Buffer, endStream = false): Buffer {
  const prefix = Buffer.alloc(5);
  prefix[0] = endStream ? FLAG_END_STREAM : 0;
  prefix.writeUInt32BE(payload.length, 1);
  return Buffer.concat([prefix, payload]);
}

/**
 * Build a Connect server-stream response body: one data frame per message
 * object, then a terminal end-stream frame. Pass an `error` to make the
 * end-stream frame carry a Connect error envelope (a typed-error case);
 * otherwise it is a clean empty trailer frame.
 */
export function streamBody(
  messages: Array<Record<string, unknown>>,
  error?: { code: string; message: string },
): Buffer {
  const frames = messages.map((m) => encodeFrame(Buffer.from(JSON.stringify(m), "utf-8")));
  const endPayload = error ? JSON.stringify({ error }) : "{}";
  frames.push(encodeFrame(Buffer.from(endPayload, "utf-8"), true));
  return Buffer.concat(frames);
}

/**
 * Decode a buffer of Connect enveloped request frames into their parsed JSON
 * payloads. The SDK sends its request message(s) as plain (flag 0x00) frames; an
 * end-stream frame, if present, is skipped.
 */
export function decodeFrames(buf: Buffer): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  let off = 0;
  while (off + 5 <= buf.length) {
    const flag = buf[off];
    const len = buf.readUInt32BE(off + 1);
    const payload = buf.subarray(off + 5, off + 5 + len);
    off += 5 + len;
    if (flag & FLAG_END_STREAM) {
      continue;
    }
    out.push(JSON.parse(payload.toString("utf-8")) as Record<string, unknown>);
  }
  return out;
}
