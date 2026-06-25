# frozen_string_literal: true

require "mitos/errors"

module Mitos
  # Info about a single committed WorkspaceRevision, as reported by Workspace#log.
  # Mirrors the Python RevisionInfo dataclass.
  RevisionInfo = Struct.new(
    :name, :phase, :lineage, :resumable, :created, keyword_init: true
  )

  # A content diff summary for a revision, as reported by Workspace#diff. Mirrors
  # the Python DiffInfo dataclass.
  DiffInfo = Struct.new(:parent, :added, :removed, :modified, keyword_init: true)

  # A durable, forkable agent workspace handle. Lazy: it does not touch the
  # cluster until a verb is called. Verbs are git-shaped (log, diff, fork,
  # revert). The Ruby port of the Python Workspace (sdk/python/mitos/workspace.py).
  class Workspace
    attr_reader :name, :namespace

    def initialize(name, namespace, k8s)
      @name = name
      @namespace = namespace
      @k8s = k8s
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
