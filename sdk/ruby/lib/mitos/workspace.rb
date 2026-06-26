# frozen_string_literal: true

require "mitos/errors"
require "securerandom"

module Mitos
  # Info about a single committed WorkspaceRevision, as reported by Workspace#log.
  # Mirrors the Python RevisionInfo dataclass.
  RevisionInfo = Struct.new(
    :name, :phase, :lineage, :resumable, :created, keyword_init: true
  )

  # A content diff summary for a revision, as reported by Workspace#diff. Mirrors
  # the Python DiffInfo dataclass.
  DiffInfo = Struct.new(:parent, :added, :removed, :modified, keyword_init: true)

  # ServedWorkspace is the handle returned by Workspace#serve. It carries the
  # public HTTPS URL and the identity of the backing sandbox. Mirrors the Go
  # ServedWorkspace struct (sdk/go/serve.go).
  ServedWorkspace = Struct.new(:url, :sandbox_name, :label, :sharing, keyword_init: true)

  # A durable, forkable agent workspace handle. Lazy: it does not touch the
  # cluster until a verb is called. Verbs are git-shaped (log, diff, fork,
  # revert). The Ruby port of the Python Workspace (sdk/python/mitos/workspace.py).
  class Workspace
    # Labels that are reserved for control-plane use. Mirrors reservedExposeLabels
    # in sdk/go/serve.go and internal/preview/route.go reservedLabels. Keep in sync
    # with the proxy when adding new reserved names.
    RESERVED_EXPOSE_LABELS = %w[
      www app api console gateway
      admin auth login account mail
      static assets cdn status
    ].freeze

    # DNS label validation: starts and ends with alphanumeric, hyphens allowed in
    # the middle, max 63 characters. Mirrors exposeLabelRE in sdk/go/serve.go.
    EXPOSE_LABEL_RE = /\A[a-z0-9]([a-z0-9-]*[a-z0-9])?\z/.freeze

    # Polling interval (seconds) between sandbox-ready checks in serve. Override
    # in tests by replacing this constant to avoid sleeping.
    SERVE_POLL_INTERVAL = 0.2

    attr_reader :name, :namespace

    def initialize(name, namespace, k8s)
      @name = name
      @namespace = namespace
      @k8s = k8s
    end

    # Expose a workspace-bound sandbox over HTTPS and return a ServedWorkspace
    # carrying the public URL. Mirrors Workspace.Serve in sdk/go/serve.go
    # (issue #312, expose slice 5b).
    #
    # pool is required. expose_domain defaults to ENV["MITOS_EXPOSE_DOMAIN"].
    # port defaults to 8080 and must be in 1..65535. sharing defaults to
    # "private". label defaults to the generated sandbox name and must match
    # the single DNS label pattern after being normalized to lowercase.
    #
    # TODO: link-token minting is a follow-up (#312).
    def serve(pool: nil, port: 8080, sharing: "private", label: nil, expose_domain: nil)
      unless pool
        raise ArgumentError, "serve requires a pool: argument; pass pool: <name> to select the SandboxPool to claim from"
      end

      unless port.is_a?(Integer) && port >= 1 && port <= 65535
        raise ArgumentError, "serve port #{port.inspect} is out of range; port must be an integer in 1..65535"
      end

      domain = expose_domain || ENV["MITOS_EXPOSE_DOMAIN"]
      unless domain && !domain.empty?
        raise ArgumentError,
              "expose_domain is required; pass expose_domain: <domain> or set the MITOS_EXPOSE_DOMAIN environment variable"
      end

      # Generate sandbox name upfront so it can serve as the default label.
      sb_name = "sandbox-#{SecureRandom.hex(4)}"

      effective_label = label ? label.to_s.downcase : sb_name
      validate_expose_label!(effective_label)

      url = "https://#{effective_label}.#{domain}/"

      body = {
        "apiVersion" => "#{API_GROUP}/#{API_VERSION}",
        "kind" => "Sandbox",
        "metadata" => { "name" => sb_name, "namespace" => @namespace },
        "spec" => {
          "source" => { "poolRef" => { "name" => pool } },
          "workspaceRef" => { "name" => @name },
          "expose" => {
            "port" => port,
            "label" => effective_label,
            "sharing" => sharing
          }
        }
      }
      @k8s.create_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", body)

      wait_sandbox_ready(sb_name)

      ServedWorkspace.new(
        url: url,
        sandbox_name: sb_name,
        label: effective_label,
        sharing: sharing
      )
    end

    # The current head revision name (status.head), or "" when unset.
    def head
      (fetch["status"] || {})["head"] || ""
    end

    # Whether the workspace head is resumable (status.resumable).
    def resumable
      ((fetch["status"] || {})["resumable"] ? true : false)
    end

    # The revision log, newest first. Each entry is a RevisionInfo. Filters the
    # WorkspaceRevision collection to this workspace by spec.workspaceRef.name.
    def log
      objs = @k8s.list_namespaced(API_GROUP, API_VERSION, @namespace, "workspacerevisions")
      revs = []
      (objs["items"] || []).each do |o|
        spec = o["spec"] || {}
        next if dig(spec, "workspaceRef", "name") != @name

        revs << RevisionInfo.new(
          name: dig(o, "metadata", "name"),
          phase: dig(o, "status", "phase") || "",
          lineage: lineage(spec),
          resumable: !spec["memorySnapshotRef"].nil?,
          created: dig(o, "metadata", "creationTimestamp") || ""
        )
      end
      revs.sort_by { |r| r.created }.reverse
    end

    # The recorded content diff for +revision+. Raises no_diff when the revision
    # carries no diff summary (it was not captured with a diff output).
    def diff(revision)
      o = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "workspacerevisions", revision)
      summary = dig(o, "status", "diffSummary")
      if summary.nil?
        raise MitosError.new(
          "revision #{revision} has no recorded diff",
          code: "no_diff",
          cause: "the revision was not captured with a diff output",
          remediation: "Terminate with the diff output enabled to record a diff."
        )
      end
      DiffInfo.new(
        parent: summary["parentRevision"] || "",
        added: summary["added"] || [],
        removed: summary["removed"] || [],
        modified: summary["modified"] || []
      )
    end

    # Branch a committed revision into dst_workspace (a content-addressed
    # branch). Returns the new revision name. dst_workspace must exist. Raises
    # revision_not_committed for an uncommitted source.
    def fork(revision, dst_workspace)
      parent = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "workspacerevisions", revision)
      manifest = dig(parent, "spec", "contentManifest") || ""
      if dig(parent, "status", "phase") != "Committed" || manifest.empty?
        raise MitosError.new(
          "cannot fork uncommitted revision #{revision}",
          code: "revision_not_committed",
          cause: "revision #{revision} is not committed",
          remediation: "Wait for the revision to commit before forking it."
        )
      end
      body = {
        "apiVersion" => "#{API_GROUP}/#{API_VERSION}", "kind" => "WorkspaceRevision",
        "metadata" => {
          "generateName" => "#{dst_workspace}-", "namespace" => @namespace,
          "labels" => { "mitos.run/workspace" => dst_workspace }
        },
        "spec" => {
          "workspaceRef" => { "name" => dst_workspace },
          "source" => { "fromWorkspaceRevision" => { "workspace" => @name, "revision" => revision } },
          "contentManifest" => manifest
        }
      }
      created = @k8s.create_namespaced(API_GROUP, API_VERSION, @namespace, "workspacerevisions", body)
      dig(created, "metadata", "name")
    end

    # Set this workspace head to a past revision by creating a new tip that
    # shares its content (revisions are immutable; a revert is a new tip).
    def revert(revision)
      fork(revision, @name)
    end

    # checkout is an alias for revert: make a past state the new head.
    alias checkout revert

    private

    # Validate a normalized (already lowercased) expose label. Raises ArgumentError
    # when the label exceeds 63 characters, fails the DNS label pattern, or is
    # reserved. Mirrors buildExposeURL in sdk/go/serve.go.
    def validate_expose_label!(label)
      if label.length > 63
        raise ArgumentError,
              "expose label #{label.inspect} exceeds 63 characters; use a shorter label (at most 63 characters)"
      end
      unless EXPOSE_LABEL_RE.match?(label)
        raise ArgumentError,
              "expose label #{label.inspect} is not a valid single DNS label; " \
              "use only lowercase letters, digits, and hyphens and do not start or end with a hyphen"
      end
      if RESERVED_EXPOSE_LABELS.include?(label)
        raise ArgumentError,
              "expose label #{label.inspect} is reserved and may not be used by tenants; " \
              "choose a different label that is not a well-known control-plane name"
      end
    end

    # Poll the sandbox until it reaches Ready or Failed. Mirrors waitSandboxReady
    # in sdk/go/serve.go. Uses SERVE_POLL_INTERVAL so tests can override it.
    def wait_sandbox_ready(name)
      loop do
        obj = @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "sandboxes", name)
        phase = (obj["status"] || {})["phase"]
        case phase
        when "Ready"
          return
        when "Failed"
          raise MitosError.new(
            "sandbox #{name} reached Failed phase",
            code: "sandbox_failed",
            cause: "the controller reported a Failed phase before Ready",
            remediation: "Check the Sandbox status for more detail."
          )
        end
        sleep self.class.const_get(:SERVE_POLL_INTERVAL)
      end
    end

    def fetch
      @k8s.get_namespaced(API_GROUP, API_VERSION, @namespace, "workspaces", @name)
    rescue ApiError => e
      raise MitosError.new(
        "workspace #{@name} not found",
        code: "workspace_not_found",
        cause: e.reason.to_s,
        remediation: "Create it with create_workspace(name) first.",
        status: e.status
      )
    end

    def lineage(spec)
      src = spec["source"] || {}
      return "fromClaim:#{src['fromClaim']}" if src["fromClaim"]

      fwr = src["fromWorkspaceRevision"]
      return "fromWorkspaceRevision:#{fwr['revision'] || ''}" if fwr

      "root"
    end

    def dig(hash, *keys)
      keys.reduce(hash) { |acc, k| acc.is_a?(Hash) ? acc[k] : nil }
    end
  end
end
