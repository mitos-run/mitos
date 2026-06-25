# frozen_string_literal: true

require "minitest/autorun"
require "webrick"
require "json"
require "socket"
require "stringio"
require "base64"
require "securerandom"

require "mitos"

# Dispatches every HTTP method (including DELETE, which WEBrick's default
# mount_proc does not route) to a single proc.
class AnyMethodServlet < WEBrick::HTTPServlet::AbstractServlet
  def initialize(server, handler)
    super(server)
    @handler = handler
  end

  def service(req, res)
    @handler.call(req, res)
  end
end

# A WEBrick stub of the Kubernetes API server, serving only the namespaced
# custom-resource and Secret endpoints the AgentRun surface touches. It keeps an
# in-memory object store keyed by "{plural}/{namespace}/{name}", records every
# request so the tests can assert path + method + body, and returns Kubernetes
# Status objects (with reason + code) for 404 / 409 so the SDK's ApiError carries
# the right status. Mirrors the Python tests' mocked CustomObjectsApi.
class K8sStub
  Recorded = Struct.new(:method, :path, :body, keyword_init: true)

  attr_reader :recorded, :base_url, :store

  def initialize
    @recorded = []
    @store = {}
    @server = WEBrick::HTTPServer.new(
      BindAddress: "127.0.0.1",
      Port: 0,
      Logger: WEBrick::Log.new(StringIO.new),
      AccessLog: []
    )
    @server.mount("/", AnyMethodServlet, method(:dispatch))
    port = @server.listeners.first.addr[1]
    @base_url = "http://127.0.0.1:#{port}"
    @thread = Thread.new { @server.start }
    wait_until_ready(port)
  end

  def stop
    @server.shutdown
    @thread.join
  end

  # Seed an object directly into the store (bypassing a POST), used to set up
  # pre-existing pools / sandboxes / workspaces for read paths.
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

  # Parse /apis/{group}/{version}/namespaces/{ns}/{plural}[/{name}] and
  # /api/v1/namespaces/{ns}/secrets/{name}. Returns a Hash or nil.
  def parse_path(path)
    if (m = path.match(%r{\A/apis/[^/]+/[^/]+/namespaces/([^/]+)/([^/]+)(?:/([^/]+))?\z}))
      { kind: :custom, namespace: m[1], plural: m[2], name: m[3] }
    elsif (m = path.match(%r{\A/api/v1/namespaces/([^/]+)/secrets/([^/]+)\z}))
      { kind: :secret, namespace: m[1], name: m[2] }
    end
  end

  def dispatch(req, res)
    body = req.body && !req.body.empty? ? JSON.parse(req.body) : nil
    @recorded << Recorded.new(method: req.request_method, path: req.path, body: body)

    route = parse_path(req.path)
    return json(res, status_obj("NotFound", 404, "unknown path"), 404) unless route

    if route[:kind] == :secret
      return serve_secret(res, route)
    end

    case req.request_method
    when "GET"  then serve_get(res, route)
    when "POST" then serve_post(res, route, body)
    when "DELETE" then serve_delete(res, route)
    else json(res, status_obj("MethodNotAllowed", 405, "method not allowed"), 405)
    end
  end

  def serve_secret(res, route)
    obj = @store[key("secrets", route[:namespace], route[:name])]
    return json(res, status_obj("NotFound", 404, "secret not found"), 404) unless obj

    json(res, obj)
  end

  def serve_get(res, route)
    if route[:name]
      obj = @store[key(route[:plural], route[:namespace], route[:name])]
      return json(res, status_obj("NotFound", 404, "#{route[:plural]} #{route[:name]} not found"), 404) unless obj

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

class DefaultPoolNameTest < Minitest::Test
  # The seven exact vectors that MUST match the Python default_pool_name byte
  # for byte (and the TypeScript defaultPoolName).
  VECTORS = {
    "python:3.12" => "mitos-default-python-3.12",
    "ghcr.io/Acme/Foo-Bar:latest" => "mitos-default-ghcr.io-acme-foo-bar-latest",
    "busybox" => "mitos-default-busybox",
    "UPPER/Case:TAG" => "mitos-default-upper-case-tag",
    ("a" * 60 + ":x") => ("mitos-default-" + "a" * 40),
    "registry.io/x@sha256:abc" => "mitos-default-registry.io-x-sha256-abc",
    "node_18" => "mitos-default-node-18"
  }.freeze

  def test_vectors
    VECTORS.each do |image, expected|
      assert_equal expected, Mitos.default_pool_name(image),
                   "default_pool_name(#{image.inspect})"
    end
  end
end

class AgentRunTest < Minitest::Test
  def setup
    @stub = K8sStub.new
    @run = Mitos::AgentRun.new(namespace: "default", client: @stub.client)
  end

  def teardown
    @stub.stop
  end

  def find(method, path_re)
    @stub.recorded.find { |r| r.method == method && r.path.match?(path_re) }
  end

  def test_sandbox_get_or_creates_default_pool_then_sandbox
    sb = @run.sandbox(image: "python:3.12")
    assert_instance_of Mitos::ClusterSandbox, sb

    # The default pool was created at the right path with an inline template.
    pool_post = find("POST", %r{\A/apis/mitos.run/v1/namespaces/default/sandboxpools\z})
    refute_nil pool_post
    assert_equal "mitos-default-python-3.12", pool_post.body["metadata"]["name"]
    assert_equal "python:3.12", pool_post.body["spec"]["template"]["image"]
    assert_equal 1, pool_post.body["spec"]["replicas"]

    # The sandbox was created with a poolRef to that pool.
    sb_post = find("POST", %r{\A/apis/mitos.run/v1/namespaces/default/sandboxes\z})
    refute_nil sb_post
    assert_equal "mitos-default-python-3.12", sb_post.body["spec"]["source"]["poolRef"]["name"]
  end

  def test_sandbox_reuses_existing_default_pool_without_recreate
    @stub.seed("sandboxpools", "default", "mitos-default-busybox", {
                 "apiVersion" => "mitos.run/v1", "kind" => "SandboxPool",
                 "metadata" => { "name" => "mitos-default-busybox" },
                 "spec" => { "template" => { "image" => "busybox" }, "replicas" => 1 }
               })
    @run.sandbox(image: "busybox")
    # No POST to sandboxpools: the existing pool was reused.
    assert_nil find("POST", %r{/sandboxpools\z})
  end

  def test_sandbox_image_collision_raises
    @stub.seed("sandboxpools", "default", "mitos-default-python-3.12", {
                 "metadata" => { "name" => "mitos-default-python-3.12" },
                 "spec" => { "template" => { "image" => "python-3.12-other" }, "replicas" => 1 }
               })
    err = assert_raises(Mitos::MitosError) { @run.sandbox(image: "python:3.12") }
    assert_equal "pool_image_mismatch", err.code
  end

  def test_create_with_pool_env_secrets_ttl_workspace
    sb = @run.create(
      pool: "my-pool", name: "sb-1",
      env: { "FOO" => "bar" },
      secrets: { "TOKEN" => %w[my-secret api-key] },
      ttl: "30m", workspace: "ws-1"
    )
    assert_equal "sb-1", sb.name
    post = find("POST", %r{/sandboxes\z})
    spec = post.body["spec"]
    assert_equal "my-pool", spec["source"]["poolRef"]["name"]
    assert_equal [{ "name" => "FOO", "value" => "bar" }], spec["env"]
    assert_equal "my-secret", spec["secrets"][0]["secretRef"]["name"]
    assert_equal "api-key", spec["secrets"][0]["secretRef"]["key"]
    assert_equal "TOKEN", spec["secrets"][0]["envVar"]
    assert_equal "30m", spec["lifetime"]["ttl"]
    assert_equal "ws-1", spec["workspaceRef"]["name"]
  end

  def test_create_generates_name_when_omitted
    sb = @run.create(pool: "p")
    assert_match(/\Asandbox-[0-9a-f]{8}\z/, sb.name)
  end

  def test_409_on_pool_create_is_tolerated
    # Pre-seed the pool so the GET 404 path is skipped is NOT what we want here:
    # simulate a race by making the GET miss but the POST conflict. Seed AFTER
    # the lookup by intercepting: simplest is to pre-store under a name that the
    # GET also finds, so instead drive create_or_reuse directly via a fresh
    # AgentRun against a store that 409s. We seed the pool so POST conflicts; the
    # GET would find it, so to exercise the 409 path we delete-after-get is not
    # possible. Instead assert that a concurrent existing object never raises.
    @stub.seed("sandboxpools", "default", "mitos-default-busybox", {
                 "metadata" => { "name" => "mitos-default-busybox" },
                 "spec" => { "template" => { "image" => "busybox" }, "replicas" => 1 }
               })
    # Reuse path: existing pool, verified image, no raise.
    assert_equal "mitos-default-busybox", @run.send(:ensure_default_pool, "busybox")
  end

  def test_409_raw_create_or_reuse_swallows_conflict
    # Seed an object so POST returns 409, then call create_or_reuse directly and
    # assert it does not raise (the AlreadyExists race is tolerated).
    @stub.seed("sandboxpools", "default", "raced", { "metadata" => { "name" => "raced" } })
    body = { "metadata" => { "name" => "raced" }, "spec" => {} }
    assert_nil @run.send(:create_or_reuse, body, "sandboxpools")
  end

  def test_get_and_from_name_read_poolref_and_status
    @stub.seed("sandboxes", "default", "sb-x", {
                 "metadata" => { "name" => "sb-x" },
                 "spec" => { "source" => { "poolRef" => { "name" => "pool-x" } } },
                 "status" => { "phase" => "Pending", "endpoint" => nil }
               })
    sb = @run.get("sb-x")
    assert_equal "pool-x", sb.pool
    assert_equal "Pending", sb.phase

    same = @run.from_name("sb-x")
    assert_equal "pool-x", same.pool
  end

  def test_get_ready_sandbox_loads_token
    @stub.seed("sandboxes", "default", "sb-ready", {
                 "metadata" => { "name" => "sb-ready" },
                 "spec" => { "source" => { "poolRef" => { "name" => "p" } } },
                 "status" => { "phase" => "Ready", "endpoint" => "1.2.3.4:9091" }
               })
    @stub.seed("secrets", "default", "sb-ready-sandbox-token", {
                 "data" => { "token" => Base64.strict_encode64("super-secret-token") }
               })
    sb = @run.get("sb-ready")
    assert_equal "Ready", sb.phase
    # The token is read into memory; assert the Secret GET happened and the
    # token VALUE never appears in the recorded request paths.
    assert find("GET", %r{/secrets/sb-ready-sandbox-token\z})
    refute(@stub.recorded.any? { |r| r.path.include?("super-secret-token") })
  end

  def test_list_filters_by_pool
    @stub.seed("sandboxes", "default", "a", {
                 "metadata" => { "name" => "a" },
                 "spec" => { "source" => { "poolRef" => { "name" => "p1" } } }, "status" => {}
               })
    @stub.seed("sandboxes", "default", "b", {
                 "metadata" => { "name" => "b" },
                 "spec" => { "source" => { "poolRef" => { "name" => "p2" } } }, "status" => {}
               })
    all = @run.list
    assert_equal 2, all.length
    only_p1 = @run.list(pool: "p1")
    assert_equal ["a"], only_p1.map(&:name)
  end

  def test_pool_status_reads_status
    @stub.seed("sandboxpools", "default", "p", {
                 "metadata" => { "name" => "p" },
                 "spec" => { "replicas" => 5 },
                 "status" => { "readySnapshots" => 3, "nodeDistribution" => { "node-a" => 2, "node-b" => 1 } }
               })
    ps = @run.pool_status("p")
    assert_equal "p", ps.name
    assert_equal 3, ps.ready_snapshots
    assert_equal 5, ps.desired
    assert_equal({ "node-a" => 2, "node-b" => 1 }, ps.node_distribution)
  end

  def test_sandbox_without_image_or_pool_raises
    err = assert_raises(Mitos::MitosError) { @run.sandbox }
    assert_equal "missing_image_or_pool", err.code
  end

  def test_default_pool_disabled_raises
    run = Mitos::AgentRun.new(namespace: "default", client: @stub.client, allow_default_pool: false)
    err = assert_raises(Mitos::MitosError) { run.sandbox(image: "python") }
    assert_equal "no_default_pool", err.code
  end

  def test_terminate_returns_workspace_ref_and_deletes
    @stub.seed("sandboxes", "default", "sb-ws", {
                 "metadata" => { "name" => "sb-ws" },
                 "spec" => { "source" => { "poolRef" => { "name" => "p" } },
                             "workspaceRef" => { "name" => "ws-9" } },
                 "status" => {}
               })
    sb = @run.get("sb-ws")
    ws = sb.terminate
    assert_equal "ws-9", ws
    assert find("DELETE", %r{/sandboxes/sb-ws\z})
  end

  def test_create_workspace_and_list
    @run.create_workspace("ws-1")
    post = find("POST", %r{/workspaces\z})
    assert_equal "ws-1", post.body["metadata"]["name"]
    names = @run.list_workspaces.map(&:name)
    assert_includes names, "ws-1"
  end

  def test_get_workspace_missing_raises_typed_error
    err = assert_raises(Mitos::MitosError) { @run.get_workspace("nope") }
    assert_equal "workspace_not_found", err.code
  end
end
