# frozen_string_literal: true

require "minitest/autorun"
require "webrick"
require "json"
require "socket"
require "stringio"
require "securerandom"

require "mitos"

# Reuse the K8sStub and AnyMethodServlet pattern from cluster_test.rb.
# Duplicating here keeps the file self-contained and avoids coupling test files.

class ServeAnyMethodServlet < WEBrick::HTTPServlet::AbstractServlet
  def initialize(server, handler)
    super(server)
    @handler = handler
  end

  def service(req, res)
    @handler.call(req, res)
  end
end

# A minimal K8s stub for workspace serve tests. Stores objects in memory, records
# requests, and auto-transitions sandboxes to Ready after the first GET.
class ServeK8sStub
  Recorded = Struct.new(:method, :path, :body, keyword_init: true)

  attr_reader :recorded, :base_url, :store

  def initialize
    @recorded = []
    @store = {}
    # Track how many GETs a sandbox has received so we can auto-flip to Ready.
    @sandbox_get_counts = {}
    @server = WEBrick::HTTPServer.new(
      BindAddress: "127.0.0.1",
      Port: 0,
      Logger: WEBrick::Log.new(StringIO.new),
      AccessLog: []
    )
    @server.mount("/", ServeAnyMethodServlet, method(:dispatch))
    port = @server.listeners.first.addr[1]
    @base_url = "http://127.0.0.1:#{port}"
    @thread = Thread.new { @server.start }
    wait_until_ready(port)
  end

  def stop
    @server.shutdown
    @thread.join
  end

  def seed(plural, namespace, name, object)
    @store[key(plural, namespace, name)] = object
  end

  def client
    Mitos::K8sClient.new(server: @base_url, namespace: "default")
  end

  private

  def key(plural, namespace, name)
    "#{plural}/#{namespace}/#{name}"
  end

  def wait_until_ready(port)
    20.times do
      TCPSocket.new("127.0.0.1", port).close
      return
    rescue StandardError
      sleep 0.05
    end
  end

  def status_obj(reason, code, message)
    { "kind" => "Status", "apiVersion" => "v1", "status" => "Failure",
      "reason" => reason, "code" => code, "message" => message }
  end

  def json(res, value, code = 200)
    res.status = code
    res["Content-Type"] = "application/json"
    res.body = JSON.generate(value)
  end

  def parse_path(path)
    if (m = path.match(%r{\A/apis/[^/]+/[^/]+/namespaces/([^/]+)/([^/]+)(?:/([^/]+))?\z}))
      { kind: :custom, namespace: m[1], plural: m[2], name: m[3] }
    end
  end

  def dispatch(req, res)
    body = req.body && !req.body.empty? ? JSON.parse(req.body) : nil
    @recorded << Recorded.new(method: req.request_method, path: req.path, body: body)

    route = parse_path(req.path)
    return json(res, status_obj("NotFound", 404, "unknown path"), 404) unless route

    case req.request_method
    when "GET"  then serve_get(res, route)
    when "POST" then serve_post(res, route, body)
    when "DELETE" then serve_delete(res, route)
    else json(res, status_obj("MethodNotAllowed", 405, "method not allowed"), 405)
    end
  end

  def serve_get(res, route)
    if route[:name]
      k = key(route[:plural], route[:namespace], route[:name])
      obj = @store[k]
      return json(res, status_obj("NotFound", 404, "not found"), 404) unless obj

      # Auto-transition sandboxes to Ready on the first GET after creation.
      if route[:plural] == "sandboxes"
        count = (@sandbox_get_counts[k] ||= 0)
        @sandbox_get_counts[k] += 1
        if count >= 1
          obj = obj.dup
          obj["status"] = { "phase" => "Ready", "endpoint" => "1.2.3.4:9091" }
        end
      end
      json(res, obj)
    else
      items = @store.select { |k, _| k.start_with?("#{route[:plural]}/#{route[:namespace]}/") }.values
      json(res, { "apiVersion" => "v1", "kind" => "List", "items" => items })
    end
  end

  def serve_post(res, route, body)
    name = (body["metadata"] || {})["name"]
    name ||= "#{(body['metadata'] || {})['generateName']}#{SecureRandom.hex(3)}" if (body["metadata"] || {})["generateName"]
    k = key(route[:plural], route[:namespace], name)
    if @store.key?(k)
      return json(res, status_obj("AlreadyExists", 409, "#{route[:plural]} #{name} already exists"), 409)
    end

    stored = body.dup
    (stored["metadata"] ||= {})["name"] = name
    @store[k] = stored
    json(res, stored, 201)
  end

  def serve_delete(res, route)
    k = key(route[:plural], route[:namespace], route[:name])
    obj = @store.delete(k)
    return json(res, status_obj("NotFound", 404, "not found"), 404) unless obj

    json(res, obj)
  end
end

class WorkspaceServeTest < Minitest::Test
  def setup
    @stub = ServeK8sStub.new
    @k8s = @stub.client
    # Seed the workspace so fetch does not 404 when workspace internals call it.
    @stub.seed("workspaces", "default", "ws-1", {
                 "apiVersion" => "mitos.run/v1", "kind" => "Workspace",
                 "metadata" => { "name" => "ws-1", "namespace" => "default" },
                 "spec" => {}, "status" => {}
               })
    @ws = Mitos::Workspace.new("ws-1", "default", @k8s)
    # Disable polling sleep by overriding the interval constant at the class level.
    # The constant is defined by the implementation; guard in case tests run before
    # the constant exists (error state shows the test is correctly RED).
    if Mitos::Workspace.const_defined?(:SERVE_POLL_INTERVAL)
      @orig_poll = Mitos::Workspace.const_get(:SERVE_POLL_INTERVAL)
      Mitos::Workspace.send(:remove_const, :SERVE_POLL_INTERVAL)
      Mitos::Workspace.const_set(:SERVE_POLL_INTERVAL, 0)
    else
      @orig_poll = nil
    end
  end

  def teardown
    @stub.stop
    if Mitos::Workspace.const_defined?(:SERVE_POLL_INTERVAL)
      Mitos::Workspace.send(:remove_const, :SERVE_POLL_INTERVAL)
    end
    Mitos::Workspace.const_set(:SERVE_POLL_INTERVAL, @orig_poll) unless @orig_poll.nil?
  end

  # --- helper ---

  def find(method, path_re)
    @stub.recorded.find { |r| r.method == method && r.path.match?(path_re) }
  end

  # --- happy path: default label (sandbox name) ---

  def test_serve_happy_path_returns_served_workspace
    result = @ws.serve(pool: "python", expose_domain: "mitos.app")

    assert_instance_of Mitos::ServedWorkspace, result
    assert_match(%r{\Ahttps://[a-z0-9][a-z0-9-]*\.mitos\.app/\z}, result.url)
    assert_match(/\Asandbox-[0-9a-f]{8}\z/, result.sandbox_name)
    assert_equal result.label, result.sandbox_name
    assert_equal "private", result.sharing
  end

  def test_serve_happy_path_posts_sandbox_with_expose_and_workspace_ref
    @ws.serve(pool: "python", expose_domain: "mitos.app")

    post = find("POST", %r{/sandboxes\z})
    refute_nil post, "expected a POST to /sandboxes"
    spec = post.body["spec"]
    assert_equal "python", spec["source"]["poolRef"]["name"]
    assert_equal "ws-1", spec["workspaceRef"]["name"]
    expose = spec["expose"]
    refute_nil expose, "expected spec.expose to be set"
    assert_equal 8080, expose["port"]
    assert_equal "private", expose["sharing"]
    # label in the posted spec must match the sandbox name.
    assert_match(/\Asandbox-[0-9a-f]{8}\z/, expose["label"])
  end

  def test_serve_url_uses_sandbox_name_as_label_when_no_label_given
    result = @ws.serve(pool: "python", expose_domain: "mitos.app")
    expected = "https://#{result.sandbox_name}.mitos.app/"
    assert_equal expected, result.url
  end

  # --- custom label ---

  def test_serve_custom_label_returns_correct_url
    result = @ws.serve(pool: "python", expose_domain: "mitos.app", label: "custom-label")
    assert_equal "https://custom-label.mitos.app/", result.url
    assert_equal "custom-label", result.label
  end

  def test_serve_custom_port_and_sharing_are_reflected
    result = @ws.serve(pool: "python", expose_domain: "mitos.app",
                       label: "my-agent", port: 3000, sharing: "link")
    assert_equal "link", result.sharing

    post = find("POST", %r{/sandboxes\z})
    assert_equal 3000, post.body["spec"]["expose"]["port"]
    assert_equal "link", post.body["spec"]["expose"]["sharing"]
  end

  # --- expose_domain from env ---

  def test_serve_expose_domain_from_env
    ENV["MITOS_EXPOSE_DOMAIN"] = "example.com"
    result = @ws.serve(pool: "python", label: "my-label")
    assert_equal "https://my-label.example.com/", result.url
  ensure
    ENV.delete("MITOS_EXPOSE_DOMAIN")
  end

  # --- error: missing pool ---

  def test_serve_missing_pool_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(expose_domain: "mitos.app") }
    assert_match(/pool/, err.message)
  end

  # --- error: missing expose_domain (no kwarg, no ENV) ---

  def test_serve_missing_expose_domain_raises_argument_error
    ENV.delete("MITOS_EXPOSE_DOMAIN")
    err = assert_raises(ArgumentError) { @ws.serve(pool: "python") }
    assert_match(/expose.domain/i, err.message)
  end

  # --- error: bad port ---

  def test_serve_port_zero_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", port: 0) }
    assert_match(/port/, err.message)
  end

  def test_serve_port_too_high_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", port: 65536) }
    assert_match(/port/, err.message)
  end

  # --- error: reserved label ---

  def test_serve_reserved_label_www_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", label: "www") }
    assert_match(/reserved/, err.message)
  end

  def test_serve_reserved_labels_full_set
    %w[app api console admin auth login account mail static assets cdn status gateway].each do |lbl|
      err = assert_raises(ArgumentError, "expected #{lbl} to be reserved") do
        @ws.serve(pool: "p", expose_domain: "x.com", label: lbl)
      end
      assert_match(/reserved/, err.message)
    end
  end

  # --- error: bad label format ---

  def test_serve_bad_label_starts_with_hyphen_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", label: "-bad") }
    assert_match(/label/, err.message)
  end

  def test_serve_bad_label_ends_with_hyphen_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", label: "bad-") }
    assert_match(/label/, err.message)
  end

  def test_serve_bad_label_uppercase_is_normalized_and_passes
    # The spec says normalize to lowercase first; "MyLabel" becomes "mylabel", which is valid.
    result = @ws.serve(pool: "python", expose_domain: "mitos.app", label: "MyLabel")
    assert_equal "https://mylabel.mitos.app/", result.url
  end

  def test_serve_label_too_long_raises_argument_error
    err = assert_raises(ArgumentError) { @ws.serve(pool: "p", expose_domain: "x.com", label: "a" * 64) }
    assert_match(/label/, err.message)
  end

  def test_serve_label_max_63_chars_passes
    label = "a" * 63
    result = @ws.serve(pool: "python", expose_domain: "mitos.app", label: label)
    assert_equal "https://#{label}.mitos.app/", result.url
  end
end
