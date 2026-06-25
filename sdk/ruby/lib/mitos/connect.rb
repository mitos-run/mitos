# frozen_string_literal: true

require "json"
require "net/http"
require "uri"

require "mitos/errors"

module Mitos
  # A dependency-free client for the Connect "sandbox.v1.Sandbox" runtime
  # protocol (issue #24), spoken over the Ruby standard library (net/http, json).
  # It mirrors the Go SDK's connect.go and the Python SDK's _connect.py: the
  # proto-JSON message shapes come straight from proto/sandbox/v1/sandbox.proto
  # (camelCase field names; bytes fields are base64 strings).
  #
  # Only the server-streaming shape used by ExecStream is implemented here: the
  # client sends ONE request message as a single enveloped frame, then reads a
  # stream of enveloped response frames.
  #
  # An enveloped frame is a 5-byte prefix (1 flag byte + 4-byte big-endian
  # uint32 length) followed by that many JSON payload bytes:
  #
  #   - flag & 0x01 (compressed): refused. The SDK negotiates identity encoding
  #     and never expects a compressed frame.
  #   - flag & 0x02 (end-stream): the terminal frame. Its JSON payload carries
  #     trailers and, on failure, an {"error":{code,message}} object that becomes
  #     a typed MitosError. A clean payload ends the stream.
  #   - else (data frame, flag 0x00): the JSON payload is a proto-JSON response
  #     message yielded to the caller.
  #
  # The bearer token rides on Authorization and is never logged; it is redacted
  # from any error cause before it surfaces.
  class ConnectClient
    SERVICE_NAME = "sandbox.v1.Sandbox"
    STREAM_CONTENT_TYPE = "application/connect+json"
    SANDBOX_ID_HEADER = "X-Sandbox-Id"

    # The end-stream flag (bit 1). The final server frame sets it; its payload
    # carries trailers and an optional error object.
    FLAG_END_STREAM = 0x02
    # The compressed flag (bit 0). The SDK negotiates identity encoding and
    # rejects a compressed response frame, so this is only used to refuse one.
    FLAG_COMPRESSED = 0x01

    # Guard the frame-length prefix so a malformed or hostile length cannot make
    # the SDK allocate unbounded memory.
    MAX_FRAME_BYTES = 64 * 1024 * 1024 # 64 MiB

    # Map the Connect textual error codes to the HTTP-ish status the SDK's
    # typed-error layer keys remediation on. An unmapped code falls back to 500.
    # Mirrors the Go and Python maps.
    CODE_STATUS = {
      "canceled" => 499,
      "unknown" => 500,
      "invalid_argument" => 400,
      "deadline_exceeded" => 504,
      "not_found" => 404,
      "already_exists" => 409,
      "permission_denied" => 403,
      "resource_exhausted" => 429,
      "failed_precondition" => 400,
      "aborted" => 409,
      "out_of_range" => 400,
      "unimplemented" => 501,
      "internal" => 500,
      "unavailable" => 503,
      "data_loss" => 500,
      "unauthenticated" => 401
    }.freeze

    # base_url is the server origin (no trailing slash needed); sandbox_id rides
    # on X-Sandbox-Id; token, when present, rides on Authorization: Bearer.
    def initialize(base_url:, sandbox_id:, token: nil)
      @base_url = base_url.sub(%r{/+\z}, "")
      @sandbox_id = sandbox_id
      @token = token
    end

    # Opens a server-streaming Connect call to +method+ (e.g. "ExecStream"),
    # sending +message+ (a proto-JSON Hash) as the single opening frame. Yields
    # each response message (a parsed Hash) the instant its frame arrives. The
    # terminal end-stream frame is consumed here: a clean end ends the stream; an
    # error end raises a typed MitosError. A non-2xx status on open raises a
    # typed MitosError from the Connect unary error envelope.
    def server_stream(method, message)
      uri = URI.join(@base_url + "/", path(method).sub(%r{\A/}, ""))
      req = Net::HTTP::Post.new(uri)
      req["Content-Type"] = STREAM_CONTENT_TYPE
      req["Connect-Protocol-Version"] = "1"
      req[SANDBOX_ID_HEADER] = @sandbox_id
      req["Authorization"] = "Bearer #{@token}" if @token && !@token.empty?
      req.body = encode_frame(JSON.generate(message))

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = (uri.scheme == "https")

      http.request(req) do |resp|
        status = resp.code.to_i
        unless status >= 200 && status < 300
          raise error_from_body(status, resp.body.to_s)
        end

        buffer = +"".b
        ended = false
        resp.read_body do |chunk|
          next if chunk.nil? || chunk.empty?

          buffer << chunk.b
          loop do
            break if buffer.bytesize < 5

            length = buffer.byteslice(1, 4).unpack1("N")
            if length > MAX_FRAME_BYTES
              raise MitosError.new(
                "connect: response frame too large",
                code: "internal_error",
                cause: "frame length #{length} exceeds #{MAX_FRAME_BYTES} bytes",
                remediation: "Report this; the SDK negotiates identity encoding and bounded frames.",
                status: 500
              )
            end
            break if buffer.bytesize < 5 + length

            flag = buffer.getbyte(0)
            payload = buffer.byteslice(5, length)
            buffer = buffer.byteslice(5 + length, buffer.bytesize - (5 + length)) || +"".b

            if (flag & FLAG_COMPRESSED) != 0
              raise MitosError.new(
                "connect: unexpected compressed response frame",
                code: "internal_error",
                cause: "the SDK negotiates identity encoding and does not accept compressed frames",
                remediation: "Report this; the server should not compress when identity is negotiated.",
                status: 500
              )
            end

            if (flag & FLAG_END_STREAM) != 0
              handle_end_stream(payload)
              ended = true
              break
            end

            next if payload.empty?

            yield JSON.parse(payload)
          end
          break if ended
        end
      end
    end

    private

    def path(method)
      "/#{SERVICE_NAME}/#{method}"
    end

    # Wraps one payload in the Connect 5-byte envelope prefix (flag 0x00 + a
    # 4-byte big-endian length).
    def encode_frame(payload)
      bytes = payload.b
      [0].pack("C") + [bytes.bytesize].pack("N") + bytes
    end

    # Inspects the terminal end-stream frame. A payload carrying an
    # {"error":{code,message}} object raises a typed MitosError; a clean payload
    # (empty, trailers only, or non-JSON) returns normally.
    def handle_end_stream(payload)
      return if payload.nil? || payload.empty?

      end_msg = begin
        JSON.parse(payload)
      rescue JSON::ParserError
        return
      end
      return unless end_msg.is_a?(Hash)

      err = end_msg["error"]
      return unless err.is_a?(Hash)

      raise connect_error(err["code"].to_s, err["message"].to_s, nil)
    end

    # Builds a typed MitosError from a non-2xx Connect response. Prefers the
    # Connect unary error envelope {"code","message"}; falls back to the raw
    # (redacted) body and HTTP status when the body is not that shape.
    def error_from_body(status, raw_body)
      parsed = begin
        JSON.parse(raw_body)
      rescue JSON::ParserError
        nil
      end

      if parsed.is_a?(Hash) && parsed["code"] && !parsed["code"].to_s.empty?
        return connect_error(parsed["code"].to_s, parsed["message"].to_s, status)
      end

      MitosError.new(
        "sandbox RPC failed: HTTP #{status}",
        code: "http_error",
        cause: redact(raw_body.strip.empty? ? "HTTP #{status}" : raw_body.strip),
        remediation: "Inspect the request against the sandbox.v1.Sandbox contract.",
        status: status
      )
    end

    # Builds a typed MitosError from a Connect error code and message. The
    # Connect textual code is the stable code; the status is mapped (or the HTTP
    # status from a non-2xx open is used when given); the message is redacted of
    # any token.
    def connect_error(code, message, http_status)
      stable = code.empty? ? "internal" : code
      status = http_status || CODE_STATUS[code] || 500
      MitosError.new(
        "sandbox RPC failed: #{stable}",
        code: stable,
        cause: redact(message.empty? ? "connect error #{stable}" : message),
        remediation: "Inspect the request against the sandbox.v1.Sandbox contract.",
        status: status
      )
    end

    def redact(text)
      return text if @token.nil? || @token.empty?

      text.to_s.gsub(@token, "[REDACTED]")
    end
  end
end
