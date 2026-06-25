# frozen_string_literal: true

require "base64"
require "securerandom"

require "mitos/errors"
require "mitos/k8s"

module Mitos
  # The mitos.run CRD API group and the single stable version. Mirrors the
  # Python and TypeScript SDKs (API_GROUP / API_VERSION).
  API_GROUP = "mitos.run"
  API_VERSION = "v1"

  # Prefix for the deterministic default-pool name. Mirrors the Python
  # _DEFAULT_POOL_PREFIX and the TypeScript default-pool prefix.
  DEFAULT_POOL_PREFIX = "mitos-default-"

  # Any run of characters NOT in the safe object-name set collapses to a single
  # "-". Mirrors the Python _SLUG_RE (re.compile(r"[^a-z0-9.-]+")) byte for byte.
  SLUG_RE = /[^a-z0-9.-]+/.freeze

  # Derives a deterministic default-pool name for an image. The image is
  # lowercased, "/" and ":" become "-", any other unsafe character collapses to
  # "-", the slug is bounded to 40 characters, then leading and trailing "-" and
  # "." are stripped (a trailing "." or "-" is an invalid object-name tail), and
  # the result is prefixed with "mitos-default-". Kept byte-for-byte equivalent
  # to the Python default_pool_name and the TypeScript defaultPoolName so the
  # same image maps to the same default pool across every SDK.
  def self.default_pool_name(image)
    slug = image.to_s.downcase.tr("/", "-").tr(":", "-")
    slug = slug.gsub(SLUG_RE, "-")
    # Bound first, then strip trailing/leading "-" and "." so truncation can
    # never leave a name ending in "." or "-" (both invalid object-name tails).
    slug = slug[0, 40] || ""
    slug = slug.gsub(/\A[-.]+/, "").gsub(/[-.]+\z/, "")
    DEFAULT_POOL_PREFIX + slug
  end

  # The status of a SandboxPool as reported by its .status. Mirrors the Python
  # PoolStatus dataclass.
  PoolStatus = Struct.new(
    :name, :ready_snapshots, :desired, :node_distribution, keyword_init: true
  )

  # AgentRun is the Kubernetes cluster-mode client: it drives the mitos.run CRDs
  # (SandboxPool, Sandbox, Workspace) over the Kubernetes REST API. It is the
  # Ruby port of the Python AgentRun (sdk/python/mitos/client.py) and the
  # TypeScript AgentRun, and it stays dependency free by talking to the API
  # server through the stdlib-only Mitos::K8sClient.
  #
  # Direct mode (Mitos.server / Mitos::SandboxServer) is unrelated: it speaks the
  # standalone sandbox-server REST API and needs no Kubernetes.
  #
  #   run = Mitos::AgentRun.new(namespace: "agents")     # from ~/.kube/config
  #   sb  = run.sandbox(image: "python:3.12")            # lazy default pool
  #   sb  = run.create(pool: "my-pool", ttl: "30m")      # explicit pool
  #   run.list(pool: "my-pool")                          # reconnect handles
  class AgentRun
    attr_reader :namespace

    # Build the client. With in_cluster: true the configuration comes from the
    # pod service-account mount; otherwise it comes from a kubeconfig (the
    # kubeconfig: path, else KUBECONFIG, else ~/.kube/config). namespace defaults
    # to the kubeconfig / in-cluster namespace when set, else "default". Pass a
    # ready-made client: as Mitos::K8sClient for tests. allow_default_pool gates
    # the lazy sandbox(image:) path.
    def initialize(namespace: nil, kubeconfig: nil, in_cluster: false,
                   allow_default_pool: true, client: nil)
      @k8s = client || (in_cluster ? K8sClient.in_cluster : K8sClient.from_kubeconfig(kubeconfig))
      @namespace = namespace || @k8s.default_namespace || "default"
      @allow_default_pool = allow_default_pool
    end

    # The one-liner entry point. Pass image: for the lazy path: ensure a default
    # pool named mitos-default-<image-slug> exists (creating it with an inline
    # template if absent and allowed), then start a Sandbox from it. Pass pool:
    # for the explicit path, which never creates a pool. Exactly one of image: or
    # pool: is required. With ready: true the call blocks until the sandbox is
    # Ready (or raises); with ready: false (default) the first exec lazily waits.
    def sandbox(image: nil, pool: nil, name: nil, env: nil, secrets: nil,
                ttl: nil, workspace: nil, ready: false)
      if pool.nil? && image.nil?
        raise MitosError.new(
          "sandbox needs an image or a pool",
          code: "missing_image_or_pool",
          remediation: 'Pass image: "python" for a lazy default pool, or pool: "my-pool" for an existing pool.'
        )
      end
      if pool.nil?
        unless @allow_default_pool
          raise MitosError.new(
            "default pools are disabled on this client",
            code: "no_default_pool",
            remediation: "Pass pool: <name> for an existing pool, or construct AgentRun(allow_default_pool: true)."
          )
        end
        pool = ensure_default_pool(image)
      end

      sb = create(pool: pool, name: name, env: env, secrets: secrets, ttl: ttl, workspace: workspace)
      sb.wait_until_ready if ready
      sb
    end

    # Create a sandbox from a pool. name is generated when omitted. env injects
    # plain environment variables; secrets maps an env var name to a
    # [secret_name, secret_key] pair sourced from a Kubernetes Secret; ttl sets
    # the maximum lifetime (for example "30m", "1h"); workspace binds the sandbox
    # to a durable Workspace by name. Returns a ClusterSandbox handle.
    def create(pool:, name: nil, env: nil, secrets: nil, ttl: nil, workspace: nil)
      name ||= "sandbox-#{SecureRandom.hex(4)}"

      spec = { "source" => { "poolRef" => { "name" => pool } } }
      if env && !env.empty?
        spec["env"] = env.map { |k, v| { "name" => k.to_s, "value" => v.to_s } }
      end
      if secrets && !secrets.empty?
        spec["secrets"] = secrets.map do |env_var, (secret_name, secret_key)|
          {
            "name" => env_var.to_s,
            "secretRef" => { "name" => secret_name, "key" => secret_key },
            "envVar" => env_var.to_s
          }
        end
      end
      (spec["lifetime"] ||= {})["ttl"] = ttl if ttl
      spec["workspaceRef"] = { "name" => workspace } if workspace

      body = {
        "apiVersion" => "#{API_GROUP}/#{API_VERSION}",
        "kind" => "Sandbox",
        "metadata" => { "name" => name, "namespace" => @namespace },
        "spec" => spec
      }
      @k8s.create_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", body)

      ClusterSandbox.new(name: name, namespace: @namespace, pool: pool, k8s: @k8s)
    end

    # Reconnect to an existing sandbox by name, returning a live handle. Alias of
    # get, named for the durable-handle / reconnect use case.
    def from_name(name)
      get(name)
    end

    # Get an existing sandbox by name. Reads spec.source.poolRef for the pool and
    # status for the phase and endpoint, loading the per-sandbox token when Ready.
    def get(name)
      obj = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", name)
      status = obj["status"] || {}
      pool = dig(obj, "spec", "source", "poolRef", "name") || ""
      sb = ClusterSandbox.new(
        name: name, namespace: @namespace, pool: pool, k8s: @k8s,
        endpoint: status["endpoint"], phase: status["phase"] || "Pending"
      )
      sb.send(:load_token) if sb.phase == "Ready"
      sb
    end

    # List sandboxes, optionally filtered by pool (spec.source.poolRef.name).
    def list(pool: nil)
      objs = @k8s.list_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes")
      out = []
      (objs["items"] || []).each do |obj|
        obj_pool = dig(obj, "spec", "source", "poolRef", "name") || ""
        next if pool && obj_pool != pool

        status = obj["status"] || {}
        out << ClusterSandbox.new(
          name: dig(obj, "metadata", "name"), namespace: @namespace, pool: obj_pool,
          k8s: @k8s, endpoint: status["endpoint"], phase: status["phase"] || "Pending"
        )
      end
      out
    end

    # Create an empty durable Workspace.
    def create_workspace(name)
      body = {
        "apiVersion" => "#{API_GROUP}/#{API_VERSION}", "kind" => "Workspace",
        "metadata" => { "name" => name, "namespace" => @namespace }, "spec" => {}
      }
      @k8s.create_namespaced(API_GROUP, API_VERSION, @namespace, "workspaces", body)
      Workspace.new(name, @namespace, @k8s)
    end

    # A lazy handle to a workspace; it does not touch the cluster until a verb is
    # called. Use create_workspace when the workspace must be created.
    def workspace(name)
      Workspace.new(name, @namespace, @k8s)
    end

    # Reconnect to an existing workspace, raising workspace_not_found if absent.
    def get_workspace(name)
      ws = Workspace.new(name, @namespace, @k8s)
      ws.send(:fetch) # raises workspace_not_found when absent
      ws
    end

    # List the workspaces in the client's namespace as lazy handles.
    def list_workspaces
      objs = @k8s.list_namespaced(API_GROUP, API_VERSION, @namespace, "workspaces")
      (objs["items"] || []).map { |o| Workspace.new(dig(o, "metadata", "name"), @namespace, @k8s) }
    end

    # Get the status of a SandboxPool: ready snapshots, desired replicas, and the
    # per-node distribution.
    def pool_status(name)
      obj = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxpools", name)
      status = obj["status"] || {}
      spec = obj["spec"] || {}
      PoolStatus.new(
        name: name,
        ready_snapshots: status["readySnapshots"] || 0,
        desired: spec["replicas"] || 0,
        node_distribution: status["nodeDistribution"] || {}
      )
    end

    private

    # get-or-create the default SandboxPool for an image; returns the pool name.
    # A pre-existing pool is reused after verifying its inline image matches; a
    # missing one is created as a single SandboxPool with an inline
    # spec.template (v1 has no separate SandboxTemplate object). A concurrent
    # creator's 409 is tolerated and the existing pool reused.
    def ensure_default_pool(image)
      name = Mitos.default_pool_name(image)
      begin
        existing = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxpools", name)
        verify_pool_image(existing, name, image)
        return name
      rescue ApiError => e
        raise unless e.status == 404
      end

      pool = {
        "apiVersion" => "#{API_GROUP}/#{API_VERSION}",
        "kind" => "SandboxPool",
        "metadata" => { "name" => name, "namespace" => @namespace },
        "spec" => { "template" => { "image" => image }, "replicas" => 1 }
      }
      create_or_reuse(pool, "sandboxpools")
      name
    end

    # Guard the default-pool reuse path against a slug collision serving the
    # wrong image. Two distinct images can normalize to one default pool (for
    # example "python:3.11" and "python-3.11"), so reading the inline
    # spec.template.image and comparing it to the requested image proves a reused
    # pool runs the requested image; a mismatch or an unreadable image fails
    # closed rather than silently running the first caller's image.
    def verify_pool_image(pool, name, image)
      existing = dig(pool, "spec", "template", "image")
      if existing.nil? || existing.to_s.empty?
        raise MitosError.new(
          "default pool #{name} has no readable inline template image",
          code: "pool_image_mismatch",
          cause: "pool #{name} spec.template.image is absent or unreadable",
          remediation: "Pass pool: #{name.inspect} explicitly to reuse this pool, or use a distinct image."
        )
      end
      return if existing == image

      raise MitosError.new(
        "default pool #{name} already exists for a different image",
        code: "pool_image_mismatch",
        cause: "pool #{name} runs image #{existing.inspect}, not the requested #{image.inspect} (the image slug collides)",
        remediation: "Pass pool: #{name.inspect} explicitly to reuse this pool, or use a distinct image."
      )
    end

    # Create a namespaced custom object, tolerating a 409 from a concurrent
    # creator (the object is reused untouched).
    def create_or_reuse(body, plural)
      @k8s.create_namespaced(API_GROUP, API_VERSION, @namespace, plural, body)
    rescue ApiError => e
      raise unless e.status == 409 # raced another creator; reuse it
    end

    # dig that tolerates nil intermediate Hashes from the parsed JSON.
    def dig(hash, *keys)
      keys.reduce(hash) { |acc, k| acc.is_a?(Hash) ? acc[k] : nil }
    end
  end
end
