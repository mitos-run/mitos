# frozen_string_literal: true

require "base64"

require "mitos/errors"

module Mitos
  # A cluster-mode sandbox handle returned by AgentRun. It is the Ruby port of
  # the Python Sandbox (sdk/python/mitos/sandbox.py) reduced to the lifecycle
  # surface the dependency-free gem ships: it resolves phase, endpoint, and the
  # per-sandbox bearer token from the cluster, waits for Ready, reports info, and
  # terminates. exec / files / run_code over the sandbox HTTP API are served by
  # the Python and TypeScript SDKs and are out of scope here (see README).
  #
  # The per-sandbox token is read from the <name>-sandbox-token Secret, held in
  # memory only, and never logged or placed in any error message.
  class ClusterSandbox
    # How often wait_until_ready polls the Sandbox status, in seconds.
    POLL_INTERVAL = 0.2

    attr_reader :name, :namespace, :pool

    def initialize(name:, namespace:, pool:, k8s:, endpoint: nil, phase: "Pending")
      @name = name
      @namespace = namespace
      @pool = pool
      @k8s = k8s
      @endpoint = endpoint
      @phase = phase
      @sandbox_id = nil
      @token = nil
    end

    def phase
      @phase
    end

    # The sandbox HTTP endpoint, resolving readiness first if it is not yet
    # known. Raises ready_timeout / sandbox_failed via wait_until_ready.
    def endpoint
      wait_until_ready unless @endpoint
      @endpoint
    end

    # Block until the sandbox is Ready, then return self so it chains. Raises a
    # MitosError with code sandbox_failed or ready_timeout otherwise. Idempotent:
    # returns immediately when already Ready with an endpoint.
    def wait_until_ready(timeout: 30.0)
      return self if @phase == "Ready" && @endpoint

      deadline = monotonic + timeout
      while monotonic < deadline
        obj = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", @name)
        status = obj["status"] || {}
        @phase = status["phase"] || "Pending"
        @endpoint = status["endpoint"]
        @sandbox_id = status["sandboxID"]

        if @phase == "Ready" && @endpoint
          load_token
          return self
        end
        if @phase == "Failed"
          raise MitosError.new(
            "sandbox #{@name} failed",
            code: "sandbox_failed",
            cause: "sandbox #{@name} reached the Failed phase",
            remediation: "Inspect the Sandbox status conditions and the pool capacity."
          )
        end
        sleep POLL_INTERVAL
      end

      raise MitosError.new(
        "sandbox #{@name} not ready after #{timeout}s",
        code: "ready_timeout",
        cause: "sandbox #{@name} did not reach Ready within #{timeout}s",
        remediation: "Raise the timeout, or check the controller is reconciling and the pool has capacity."
      )
    end

    # Current sandbox info from the cluster: phase, endpoint, node, sandbox id,
    # startup latency, and pool.
    def info
      obj = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", @name)
      status = obj["status"] || {}
      SandboxInfo.new(
        name: @name,
        phase: status["phase"] || "Pending",
        endpoint: status["endpoint"] || "",
        node: status["node"] || "",
        sandbox_id: status["sandboxID"] || "",
        fork_time_ms: status["startupLatencyMs"] || 0,
        pool: @pool
      )
    end

    # Terminate the sandbox (DELETE the Sandbox object). When bound to a
    # Workspace the controller dehydrates /workspace into a new committed
    # revision on the way out. Returns the bound workspace name (discoverable
    # with workspace.log) or nil when the sandbox is unbound.
    def terminate
      obj = begin
        @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", @name)
      rescue ApiError => e
        raise unless e.status == 404

        {}
      end
      ws_ref = ((obj["spec"] || {})["workspaceRef"] || {})["name"]
      begin
        @k8s.delete_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", @name)
      rescue ApiError => e
        raise unless e.status == 404
      end
      @phase = "Terminating"
      ws_ref
    end

    def to_s
      "#<Mitos::ClusterSandbox name=#{@name.inspect} phase=#{@phase.inspect} endpoint=#{@endpoint.inspect}>"
    end
    alias inspect to_s

    private

    # Read the sandbox API bearer token from the <name>-sandbox-token Secret. The
    # controller creates the Secret alongside the Ready sandbox. A missing Secret
    # is tolerated: the sandbox stays tokenless and the API answers 401, which
    # surfaces the misconfiguration without crashing here. The token VALUE is
    # held in memory only and never logged.
    def load_token
      secret = begin
        @k8s.get_secret(@namespace, "#{@name}-sandbox-token")
      rescue ApiError
        return
      end
      data = secret["data"] || {}
      token_b64 = data["token"]
      @token = Base64.decode64(token_b64) if token_b64 && !token_b64.empty?
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end
  end

  # SandboxInfo is a point-in-time snapshot of a cluster sandbox's status.
  # Mirrors the Python SandboxInfo dataclass.
  SandboxInfo = Struct.new(
    :name, :phase, :endpoint, :node, :sandbox_id, :fork_time_ms, :pool, keyword_init: true
  )
end
