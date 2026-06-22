# frozen_string_literal: true

require "json"
require "net/http"
require "securerandom"
require "uri"

require "mitos/errors"
require "mitos/sandbox"

module Mitos
  # Client for the standalone / hosted sandbox-server REST API (direct mode, no
  # Kubernetes). Mirrors the Python SandboxServer (sdk/python/mitos/direct.py)
  # and the TypeScript SandboxServer (sdk/typescript/src/server.ts).
  #
  # fork() returns a Mitos::Sandbox bound to this server: exec round-trips
  # through the server URL and terminate issues DELETE /v1/sandboxes/{id}.
  #
  # Base URL precedence: the +url+ argument, then ENV['MITOS_BASE_URL'], then the
  # hosted production endpoint https://mitos.run. The api_key (argument, else
  # ENV['MITOS_API_KEY']) is optional; when present it rides on the
  # Authorization: Bearer header. The standalone server is tokenless and ignores
  # it; the hosted front door verifies it. The key VALUE is never logged and is
  # redacted from any error body before it becomes a cause.
  class SandboxServer
    # The hosted production control plane. Used when neither a url argument nor
    # MITOS_BASE_URL is set, so the examples work without a base URL.
    DEFAULT_BASE_URL = "https://mitos.run"

    # The sandbox id allowlist: start with an alphanumeric, then up to 63
    # alphanumeric, underscore, or hyphen characters. Mirrors daemon/validate.go,
    # the Python SDK, and the TypeScript validSandboxId.
    SANDBOX_ID_RE = /\A[A-Za-z0-9][A-Za-z0-9_-]{0,63}\z/.freeze

    attr_reader :url

    def initialize(url: nil, api_key: nil)
      @url = resolve_base_url(url)
      @api_key = resolve_api_key(api_key)
    end

    # Lists the templates known to the server.
    def list_templates
      (get("/v1/templates") || []).map { |t| to_template(t) }
    end

    # Creates (or builds) a template named +id+. Sends a fresh Idempotency-Key so
    # a retried create returns the same template rather than a duplicate
    # (matching the Python and TypeScript SDKs). Returns a Template.
    def create_template(id, init_wait_seconds: 5, idempotency_key: nil)
      body = { "id" => id, "init_wait_seconds" => init_wait_seconds }
      headers = { "Idempotency-Key" => idempotency_key || new_idempotency_key }
      to_template(post("/v1/templates", body, headers))
    end

    # Forks a sandbox from a named template. When +id+ is nil a "sandbox-<hex>"
    # id is generated. The id is validated against the allowlist; an invalid id
    # raises a MitosError before any request. Sends a fresh Idempotency-Key so a
    # retried fork returns the same sandbox rather than a duplicate. Returns a
    # Mitos::Sandbox bound to this server.
    def fork(template, id: nil, idempotency_key: nil)
      sandbox_id = id || random_sandbox_id
      unless valid_sandbox_id?(sandbox_id)
        raise MitosError.new(
          "invalid sandbox id: #{sandbox_id.inspect}",
          code: "invalid_sandbox_id",
          cause: "id must match #{SANDBOX_ID_RE.source}",
          remediation: "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars."
        )
      end
      body = { "template" => template, "id" => sandbox_id }
      headers = { "Idempotency-Key" => idempotency_key || new_idempotency_key }
      data = post("/v1/fork", body, headers)
      resolved_id = (data["id"] && !data["id"].empty? ? data["id"] : sandbox_id)
      Sandbox.new(
        id: resolved_id,
        endpoint: @url,
        server: self
      )
    end

    # Lists the live sandboxes known to the server.
    def list_sandboxes
      (get("/v1/sandboxes") || []).map { |s| to_server_sandbox(s) }
    end

    # Issues DELETE /v1/sandboxes/{id}. Called by Sandbox#terminate.
    def terminate(id)
      unless valid_sandbox_id?(id)
        raise MitosError.new(
          "invalid sandbox id: #{id.inspect}",
          code: "invalid_sandbox_id",
          cause: "id must match #{SANDBOX_ID_RE.source}",
          remediation: "Terminate only ids that match the sandbox id allowlist."
        )
      end
      request(Net::HTTP::Delete, "/v1/sandboxes/#{URI.encode_www_form_component(id)}", nil, {})
      nil
    end

    # Sends a request through the sandbox-server. Exposed for Sandbox#exec, which
    # posts to /v1/exec on the same server URL.
    def request_json(method_class, path, body, extra_headers = {})
      request(method_class, path, body, extra_headers)
    end

    def valid_sandbox_id?(id)
      !id.nil? && SANDBOX_ID_RE.match?(id)
    end

    private

    def resolve_base_url(url)
      chosen = url
      chosen = ENV["MITOS_BASE_URL"] if chosen.nil? || chosen.empty?
      chosen = DEFAULT_BASE_URL if chosen.nil? || chosen.empty?
      chosen.sub(%r{/+\z}, "")
    end

    # Resolves the bearer token: the +api_key+ argument, then ENV['MITOS_API_KEY'],
    # then the CLI login credential (the token written by `mitos auth login` at
    # ~/.config/mitos/credentials.json, honoring MITOS_CONFIG_DIR), then nil
    # (tokenless, for the standalone server). The token value is never logged.
    def resolve_api_key(api_key)
      return api_key unless api_key.nil? || api_key.empty?

      env = ENV["MITOS_API_KEY"]
      return env unless env.nil? || env.empty?

      token_from_credential_file
    end

    # Returns the "token" from the CLI credential file, or nil. A missing,
    # unreadable, or non-JSON file is not an error.
    def token_from_credential_file
      path = credentials_path
      return nil unless path && File.file?(path)

      data = JSON.parse(File.read(path))
      tok = data["token"]
      tok.nil? || tok.empty? ? nil : tok
    rescue StandardError
      nil
    end

    def credentials_path
      dir = ENV["MITOS_CONFIG_DIR"]
      return File.join(dir, "credentials.json") unless dir.nil? || dir.empty?

      home = Dir.home
      home.nil? || home.empty? ? nil : File.join(home, ".config", "mitos", "credentials.json")
    rescue StandardError
      nil
    end

    def new_idempotency_key
      SecureRandom.hex(16)
    end

    def random_sandbox_id
      "sandbox-#{SecureRandom.hex(4)}"
    end

    def get(path)
      request(Net::HTTP::Get, path, nil, {})
    end

    def post(path, body, extra_headers)
      request(Net::HTTP::Post, path, body, extra_headers)
    end

    # request performs the HTTP call, attaches the optional bearer header, parses
    # the JSON response, and raises a MitosError on any non-2xx status. The
    # api_key VALUE is never logged and is redacted from the error body.
    def request(method_class, path, body, extra_headers)
      uri = URI.join(@url + "/", path.sub(%r{\A/}, ""))
      req = method_class.new(uri)
      if body
        req["Content-Type"] = "application/json"
        req.body = JSON.generate(body)
      end
      req["Authorization"] = "Bearer #{@api_key}" if @api_key && !@api_key.empty?
      extra_headers.each { |k, v| req[k] = v }

      http = Net::HTTP.new(uri.host, uri.port)
      http.use_ssl = (uri.scheme == "https")
      resp = http.request(req)

      status = resp.code.to_i
      raise error_from_response(status, resp.body) unless status >= 200 && status < 300

      text = resp.body
      return nil if text.nil? || text.empty?

      JSON.parse(text)
    end

    # error_from_response builds a MitosError from a non-2xx response. It prefers
    # the structured server envelope {error:{code,message,cause,remediation}} and
    # falls back to status-derived defaults for an older or non-mitos server. Any
    # bearer token echoed in the body is redacted before it becomes a cause.
    def error_from_response(status, raw_body)
      body = redact(raw_body.to_s)
      code = status_code(status)
      message = "sandbox API request failed: HTTP #{status} (#{code})"
      cause = body.strip.empty? ? "HTTP #{status}" : body.strip
      remediation = status_remediation(status)

      parsed = begin
        JSON.parse(body)
      rescue JSON::ParserError
        nil
      end

      if parsed.is_a?(Hash)
        err = parsed["error"]
        if err.is_a?(Hash)
          code = nonempty(err["code"]) || code
          message = nonempty(err["message"]) || message
          cause = nonempty(redact(err["cause"].to_s)) || cause
          remediation = nonempty(err["remediation"]) || remediation
        elsif err.is_a?(String)
          cause = nonempty(redact(err)) || cause
        end
      end

      MitosError.new(message, code: code, cause: cause, remediation: remediation, status: status)
    end

    def nonempty(value)
      value.nil? || value.empty? ? nil : value
    end

    def redact(text)
      return text if @api_key.nil? || @api_key.empty?

      text.gsub(@api_key, "[REDACTED]")
    end

    STATUS_CODE = {
      400 => "bad_request",
      401 => "unauthorized",
      403 => "forbidden",
      404 => "not_found",
      409 => "conflict",
      413 => "request_too_large",
      429 => "rate_limited",
      500 => "internal_error",
      503 => "unavailable"
    }.freeze

    STATUS_REMEDIATION = {
      401 => "Check the API key is set and authorizes this request.",
      403 => "Check the API key is set and authorizes this request.",
      404 => "Confirm the sandbox id exists and is Ready before calling.",
      413 => "Reduce the request payload size.",
      429 => "Back off and retry the request after a short delay."
    }.freeze

    def status_code(status)
      return STATUS_CODE[status] if STATUS_CODE.key?(status)

      status >= 500 ? "server_error" : "request_failed"
    end

    def status_remediation(status)
      return STATUS_REMEDIATION[status] if STATUS_REMEDIATION.key?(status)
      return "Retry the request; if it persists, inspect the sandbox-server logs." if status >= 500

      "Inspect the request fields against the sandbox API contract."
    end

    def to_template(data)
      Template.new(
        id: data["id"],
        ready: data["ready"],
        created_at: data["created_at"],
        creation_time_ms: data["creation_time_ms"]
      )
    end

    def to_server_sandbox(data)
      ServerSandbox.new(
        id: data["id"],
        template_id: data["template_id"],
        endpoint: data["endpoint"],
        created_at: data["created_at"],
        fork_time_ms: data["fork_time_ms"]
      )
    end
  end

  # A template as reported by the sandbox-server.
  Template = Struct.new(:id, :ready, :created_at, :creation_time_ms, keyword_init: true) do
    def ready?
      ready == true
    end
  end

  # A sandbox summary as reported by GET /v1/sandboxes.
  ServerSandbox = Struct.new(
    :id, :template_id, :endpoint, :created_at, :fork_time_ms, keyword_init: true
  )
end
