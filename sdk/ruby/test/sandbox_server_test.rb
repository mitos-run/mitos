# frozen_string_literal: true

require "minitest/autorun"
require "webrick"
require "json"
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

    mount_any("/v1/exec") do |req, res|
      body = record(req)
      if @sandbox_ids.include?(body["sandbox"])
        json(res, { "exit_code" => 0, "stdout" => "2\n", "stderr" => "", "exec_time_ms" => 5 })
      else
        json(res, {
               "error" => {
                 "code" => "not_found",
                 "message" => "sandbox not found",
                 "cause" => "no sandbox registered",
                 "remediation" => "Confirm the sandbox id exists and is Ready before calling."
               }
             }, 404)
      end
    end

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
    assert result.success?

    call = @stub.recorded.find { |r| r.path == "/v1/exec" }
    assert_equal "sbx-exec", call.body["sandbox"]
    assert_equal "print(1 + 1)", call.body["command"]
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

  def test_non_2xx_envelope_raises_mitos_error_with_parsed_code
    # Forking an unknown template id path is not modeled by the stub; instead
    # drive a 404 through exec on a never-forked sandbox via a hand-built handle.
    sandbox = Mitos::Sandbox.new(id: "never-forked", endpoint: @stub.base_url, server: server)
    err = assert_raises(Mitos::MitosError) { sandbox.exec("echo hi") }
    assert_equal "not_found", err.code
    assert_equal 404, err.status
    assert_match(/Ready/, err.remediation)
  end

  def test_bearer_header_sent_when_api_key_present
    server(api_key: "sk-test-secret").create_template("with-key")
    call = @stub.recorded.find { |r| r.method == "POST" && r.path == "/v1/templates" }
    assert_equal "Bearer sk-test-secret", call.headers["authorization"]
  end
end
