# frozen_string_literal: true

require "minitest/autorun"
require "webrick"
require "json"
require "base64"
require "socket"
require "stringio"

require "mitos"

# A WEBrick stub reproducing the sandbox-server wire shapes
# (cmd/sandbox-server/main.go), mirroring sdk/typescript/test/server.test.ts.
# It records every request so the tests can assert headers and bodies.
# AnyMethodServlet dispatches every HTTP method (including DELETE, which
# WEBrick's default ProcHandler does not route) to a single proc.
class AnyMethodServlet < WEBrick::HTTPServlet::AbstractServlet
  def initialize(server, handler)
    super(server)
    @handler = handler
  end

  def service(req, res)
    @handler.call(req, res)
  end
end

class StubServer
  Recorded = Struct.new(:method, :path, :body, :headers, keyword_init: true)

  attr_reader :recorded, :base_url

  def initialize
    @recorded = []
    @sandbox_ids = []
    @server = WEBrick::HTTPServer.new(
      BindAddress: "127.0.0.1",
      Port: 0,
      Logger: WEBrick::Log.new(StringIO.new),
      AccessLog: []
    )
    mount
    # Resolve the actual bound port BEFORE starting: with Port: 0 the OS assigns
    # an ephemeral port at listener-creation time, so the listener socket already
    # knows it even though config[:Port] stays 0 on Ruby 2.6.
    port = @server.listeners.first.addr[1]
    @base_url = "http://127.0.0.1:#{port}"
    @thread = Thread.new { @server.start }
    wait_until_ready(port)
  end

  def stop
    @server.shutdown
    @thread.join
  end

  private

  # wait_until_ready blocks until the WEBrick accept loop is serving, so the
  # first test request does not race the server start.
  def wait_until_ready(port)
    20.times do
      begin
        TCPSocket.new("127.0.0.1", port).close
        return
      rescue StandardError
        sleep 0.05
      end
    end
  end

  def record(req)
    body = req.body && !req.body.empty? ? JSON.parse(req.body) : nil
    headers = {}
    req.each { |k, v| headers[k.downcase] = v }
    @recorded << Recorded.new(method: req.request_method, path: req.path, body: body, headers: headers)
    body
  end

  def json(res, value, code = 200)
    res.status = code
    res["Content-Type"] = "application/json"
    res.body = JSON.generate(value)
  end

  def mount_any(path, &block)
    @server.mount(path, AnyMethodServlet, block)
  end

  def mount
    mount_any("/v1/templates") do |req, res|
      body = record(req)
      if req.request_method == "GET"
        json(res, [
               { "id" => "python", "ready" => true,
                 "created_at" => "2026-06-11T00:00:00Z", "creation_time_ms" => 120 }
             ])
      else
        json(res, {
               "id" => body["id"], "ready" => true,
               "created_at" => "2026-06-11T00:00:00Z", "creation_time_ms" => 100
             })
      end
    end

    mount_any("/v1/fork") do |req, res|
      body = record(req)
      @sandbox_ids << body["id"]
      json(res, {
             "id" => body["id"], "template_id" => body["template"],
             "endpoint" => "http://localhost:8080", "fork_time_ms" => 0.8
           })
    end

    # Both GET /v1/sandboxes (list) and DELETE /v1/sandboxes/{id} (terminate)
    # share the /v1/sandboxes prefix; WEBrick prefix-matches, so one handler
    # dispatches on method, mirroring the Go server's two method-scoped routes.
    mount_any("/v1/sandboxes") do |req, res|
      record(req)
      rest = req.path.sub(%r{\A/v1/sandboxes/?}, "")
      if req.request_method == "DELETE"
        @sandbox_ids.delete(rest)
        json(res, { "status" => "terminated", "id" => rest })
      else
        json(res, @sandbox_ids.map do |id|
          { "id" => id, "template_id" => "python",
            "endpoint" => "http://localhost:8080",
            "created_at" => "2026-06-11T00:00:00Z", "fork_time_ms" => 0.8 }
        end)
      end
    end

    # The Connect ExecStream RPC. The request is ONE enveloped frame whose JSON
    # payload is the proto-JSON ExecStreamRequest ({command, timeoutSeconds?}).
    # The id rides on the X-Sandbox-Id header, not the body. A known sandbox gets
    # a stdout data frame, an exit data frame, then a clean end-stream frame; an
    # unknown sandbox gets an end-stream frame carrying a Connect error object.
    mount_any("/sandbox.v1.Sandbox/ExecStream") do |req, res|
      record_connect(req)
      sandbox_id = req["X-Sandbox-Id"]
      res.status = 200
      res["Content-Type"] = "application/connect+json"
      if @sandbox_ids.include?(sandbox_id)
        out = +""
        out << connect_frame({ "stdout" => Base64.strict_encode64("2\n") })
        out << connect_frame({ "exit" => { "exitCode" => 0, "execTimeMs" => 5 } })
        out << connect_frame({}, end_stream: true)
        res.body = out
      else
        res.body = connect_frame(
          {
            "error" => {
              "code" => "not_found",
              "message" => "sandbox not found"
            }
          },
          end_stream: true
        )
      end
    end

  end

  # connect_frame wraps a proto-JSON message in the Connect 5-byte envelope
  # (1 flag byte + 4-byte big-endian length + JSON payload). end_stream sets the
  # 0x02 flag on the terminal frame.
  def connect_frame(message, end_stream: false)
    payload = JSON.generate(message).b
    flag = end_stream ? 0x02 : 0x00
    [flag].pack("C") + [payload.bytesize].pack("N") + payload
  end

  # record_connect records an ExecStream call: it unwraps the single request
  # frame to capture the proto-JSON ExecStreamRequest as the body, and records
  # the headers (so tests can assert X-Sandbox-Id and Authorization).
  def record_connect(req)
    raw = (req.body || "").b
    body = nil
    if raw.bytesize >= 5
      length = raw.byteslice(1, 4).unpack1("N")
      body = JSON.parse(raw.byteslice(5, length)) if raw.bytesize >= 5 + length
    end
    headers = {}
    req.each { |k, v| headers[k.downcase] = v }
    @recorded << Recorded.new(method: req.request_method, path: req.path, body: body, headers: headers)
  end
end

class SandboxServerTest < Minitest::Test
  def setup
    @stub = StubServer.new
  end

  def teardown
    @stub.stop
  end

  def server(api_key: nil)
    Mitos::SandboxServer.new(url: @stub.base_url, api_key: api_key)
  end

  def test_list_templates
    templates = server.list_templates
    assert_equal 1, templates.length
    assert_equal "python", templates.first.id
    assert templates.first.ready?
    assert_equal 120, templates.first.creation_time_ms
  end

  def test_create_template_returns_id_and_ready
    template = server.create_template("node", init_wait_seconds: 3)
    assert_equal "node", template.id
    assert template.ready?

    call = @stub.recorded.find { |r| r.method == "POST" && r.path == "/v1/templates" }
    assert_equal({ "id" => "node", "init_wait_seconds" => 3 }, call.body)
  end

  def test_create_template_sends_idempotency_key
    server.create_template("idem-tmpl")
    call = @stub.recorded.find { |r| r.method == "POST" && r.path == "/v1/templates" }
    refute_nil call.headers["idempotency-key"]
    refute call.headers["idempotency-key"].empty?
  end

  def test_fork_returns_sandbox_with_right_id_and_sends_idempotency_key
    sandbox = server.fork("python", id: "sbx-direct")
    assert_instance_of Mitos::Sandbox, sandbox
    assert_equal "sbx-direct", sandbox.id

    call = @stub.recorded.find { |r| r.path == "/v1/fork" }
    assert_equal "python", call.body["template"]
    assert_equal "sbx-direct", call.body["id"]
    refute_nil call.headers["idempotency-key"]
    refute call.headers["idempotency-key"].empty?
  end

  def test_fork_generates_id_when_none_given
    sandbox = server.fork("python")
    assert_match(/\Asandbox-[0-9a-f]{8}\z/, sandbox.id)
  end

  def test_fork_rejects_invalid_id
    err = assert_raises(Mitos::MitosError) do
      server.fork("python", id: "../bad")
    end
    assert_equal "invalid_sandbox_id", err.code
  end

  def test_exec_round_trips_stdout_and_exit_code
    sandbox = server.fork("python", id: "sbx-exec")
    result = sandbox.exec("print(1 + 1)")
    assert_equal 0, result.exit_code
    assert_equal "2\n", result.stdout
    assert_equal 5, result.exec_time_ms
    assert result.success?

    call = @stub.recorded.find { |r| r.path == "/sandbox.v1.Sandbox/ExecStream" }
    refute_nil call
    # The id rides on the X-Sandbox-Id header, the command on the proto-JSON body.
    assert_equal "sbx-exec", call.headers["x-sandbox-id"]
    assert_equal "print(1 + 1)", call.body["command"]
    assert_equal 30, call.body["timeoutSeconds"]
  end

  def test_exec_passes_explicit_timeout_seconds
    sandbox = server.fork("python", id: "sbx-timeout")
    sandbox.exec("sleep 1", timeout: 7)
    call = @stub.recorded.find { |r| r.path == "/sandbox.v1.Sandbox/ExecStream" }
    assert_equal 7, call.body["timeoutSeconds"]
  end

  def test_exec_sends_bearer_header_when_api_key_present
    keyed = server(api_key: "sk-exec-secret")
    sandbox = keyed.fork("python", id: "sbx-auth")
    sandbox.exec("echo hi")
    call = @stub.recorded.find { |r| r.path == "/sandbox.v1.Sandbox/ExecStream" }
    assert_equal "Bearer sk-exec-secret", call.headers["authorization"]
  end

  def test_exec_on_unknown_sandbox_raises_typed_error
    sandbox = server.fork("python", id: "sbx-known")
    sandbox.terminate
    err = assert_raises(Mitos::MitosError) { sandbox.exec("echo hi") }
    assert_equal "not_found", err.code
    assert_equal 404, err.status
  end

  def test_terminate_issues_delete
    sandbox = server.fork("python", id: "sbx-term")
    sandbox.terminate
    del = @stub.recorded.find { |r| r.method == "DELETE" }
    assert_equal "/v1/sandboxes/sbx-term", del.path

    remaining = server.list_sandboxes
    assert_nil remaining.find { |s| s.id == "sbx-term" }
  end

  def test_base_url_defaults_to_hosted_endpoint
    prev = ENV["MITOS_BASE_URL"]
    ENV.delete("MITOS_BASE_URL")
    begin
      s = Mitos::SandboxServer.new
      assert_equal "https://mitos.run", s.url
    ensure
      ENV["MITOS_BASE_URL"] = prev unless prev.nil?
    end
  end

  def test_explicit_url_beats_env_and_default
    prev = ENV["MITOS_BASE_URL"]
    ENV["MITOS_BASE_URL"] = "http://from-env:9000"
    begin
      s = Mitos::SandboxServer.new(url: "http://explicit:1234")
      assert_equal "http://explicit:1234", s.url
    ensure
      if prev.nil?
        ENV.delete("MITOS_BASE_URL")
      else
        ENV["MITOS_BASE_URL"] = prev
      end
    end
  end

  def test_env_base_url_beats_default
    prev = ENV["MITOS_BASE_URL"]
    ENV["MITOS_BASE_URL"] = "http://from-env:9000"
    begin
      s = Mitos::SandboxServer.new
      assert_equal "http://from-env:9000", s.url
    ensure
      if prev.nil?
        ENV.delete("MITOS_BASE_URL")
      else
        ENV["MITOS_BASE_URL"] = prev
      end
    end
  end

  def test_error_end_stream_frame_raises_typed_mitos_error
    # An unknown sandbox makes the stub emit an end-stream frame carrying a
    # Connect {"error":{code,message}} object; the SDK maps it to a typed error.
    sandbox = Mitos::Sandbox.new(id: "never-forked", endpoint: @stub.base_url, server: server)
    err = assert_raises(Mitos::MitosError) { sandbox.exec("echo hi") }
    assert_equal "not_found", err.code
    assert_equal 404, err.status
    assert_match(/sandbox\.v1\.Sandbox/, err.remediation)
    # The token is never echoed: there is no token here, but the cause carries
    # only the server message, not any secret.
    assert_equal "sandbox not found", err.cause_detail
  end

  def test_bearer_header_sent_when_api_key_present
    server(api_key: "sk-test-secret").create_template("with-key")
    call = @stub.recorded.find { |r| r.method == "POST" && r.path == "/v1/templates" }
    assert_equal "Bearer sk-test-secret", call.headers["authorization"]
  end

  # The bearer falls back to the CLI login credential (mitos auth login) when no
  # arg and no MITOS_API_KEY are set. Precedence: arg > MITOS_API_KEY > file.
  def with_config_dir(token)
    require "tmpdir"
    Dir.mktmpdir do |dir|
      File.write(File.join(dir, "credentials.json"), JSON.dump("token" => token, "email" => "a@b.c")) unless token.nil?
      prev_dir = ENV["MITOS_CONFIG_DIR"]
      prev_key = ENV["MITOS_API_KEY"]
      ENV["MITOS_CONFIG_DIR"] = dir
      ENV.delete("MITOS_API_KEY")
      begin
        yield
      ensure
        prev_dir.nil? ? ENV.delete("MITOS_CONFIG_DIR") : (ENV["MITOS_CONFIG_DIR"] = prev_dir)
        ENV["MITOS_API_KEY"] = prev_key unless prev_key.nil?
      end
    end
  end

  def auth_header_for(call_server)
    call_server.create_template("auth-probe")
    call = @stub.recorded.find { |r| r.method == "POST" && r.path == "/v1/templates" }
    call.headers["authorization"]
  end

  def test_token_falls_back_to_credential_file
    with_config_dir("file-tok") do
      assert_equal "Bearer file-tok", auth_header_for(server)
    end
  end

  def test_env_overrides_credential_file
    with_config_dir("file-tok") do
      ENV["MITOS_API_KEY"] = "env-tok"
      assert_equal "Bearer env-tok", auth_header_for(server)
    end
  end

  def test_arg_overrides_credential_file
    with_config_dir("file-tok") do
      assert_equal "Bearer arg-tok", auth_header_for(server(api_key: "arg-tok"))
    end
  end

  def test_no_credential_no_env_is_tokenless
    with_config_dir(nil) do
      assert_nil auth_header_for(server)
    end
  end
end
