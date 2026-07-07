"""First-class ``spec.git`` helper for a durable Workspace (issue #619).

A Workspace can declare, via ``spec.git`` (the mitos.run/v1 ``WorkspaceGit``),
the repo paths inside the workspace that get version history and the
fork-and-merge rendezvous remote, plus the optional credentials used to
authenticate a ``{git}`` rendezvous push to an external remote. Git is the merge
layer: the engine only ever pushes a branch on terminate, it never merges.

Before this helper the only way to set ``spec.git`` from the SDK was to patch the
Workspace CRD by hand. ``mitos.git(...)`` builds that spec declaratively so a
caller states repo paths (and optional push credentials) once, instead of
hand-writing a CRD patch or clone commands over exec::

    ws = agent.create_workspace(
        "feature-x",
        git=mitos.git(paths=["/workspace/repo"]),
    )

The helper is pure spec construction: it needs no server, no cluster, and no
KVM, so it is fully unit-testable.

The push-credentials token is a secret VALUE. This helper only ever records a
reference to it (a Secret name and key); it never accepts, holds, or emits the
token itself. The controller resolves the reference at push time and delivers
the token to git through an ephemeral credentials file, never on the argv and
never in a log or condition.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

from mitos.errors import AgentRunError


@dataclass
class GitSpec:
    """A declarative Workspace ``spec.git`` (the mitos.run/v1 ``WorkspaceGit``).

    Args:
        paths: Repo paths inside the workspace that get version history and the
            rendezvous remote. Required and non-empty; each entry must be a
            non-blank string.
        credentials_secret: Optional ``(secret_name, secret_key)`` naming the
            Secret key that holds the push token. The token is resolved at push
            time and never enters the SDK; only this reference is recorded.
        credentials_username: Optional basic-auth username paired with the token.
            Only meaningful together with ``credentials_secret``; defaults to the
            forge convention ``x-access-token`` on the controller side when left
            unset. Passing it without ``credentials_secret`` is a fail-fast.

    Construct through :func:`git`, which validates the arguments and returns a
    ``GitSpec``.
    """

    paths: list[str]
    credentials_secret: Optional[tuple[str, str]] = None
    credentials_username: Optional[str] = None

    def __post_init__(self) -> None:
        # Validate on every construction path, not just the git() factory, so a
        # directly constructed mitos.GitSpec (it is exported) cannot smuggle an
        # invalid spec past the fail-fast checks and on to the cluster. Normalise
        # the containers to plain list/tuple after validating.
        _validate_git_args(self.paths, self.credentials_secret, self.credentials_username)
        self.paths = list(self.paths)
        if self.credentials_secret is not None:
            self.credentials_secret = tuple(self.credentials_secret)  # type: ignore[assignment]

    def to_spec(self) -> dict:
        """Render the JSON ``spec.git`` object, omitting unset optional fields."""
        spec: dict = {"paths": list(self.paths)}
        if self.credentials_secret is not None:
            name, key = self.credentials_secret
            spec["credentialsSecretRef"] = {"name": name, "key": key}
        if self.credentials_username:
            spec["credentialsUsername"] = self.credentials_username
        return spec


def _validate_git_args(
    paths: object,
    credentials_secret: object,
    credentials_username: object,
) -> None:
    """Validate the git spec arguments, raising an LLM-legible AgentRunError.

    Shared by :func:`git` and :meth:`GitSpec.__post_init__` so the fail-fast
    checks hold on every construction path, including a directly constructed
    ``mitos.GitSpec``.
    """
    if not paths or not isinstance(paths, (list, tuple)):
        raise AgentRunError(
            "git spec needs at least one workspace path",
            code="invalid_git_paths",
            cause="paths is empty or not a list",
            remediation='Pass paths=["/workspace/repo"] naming the repo path(s) to track.',
        )
    for p in paths:
        if not isinstance(p, str) or not p.strip():
            raise AgentRunError(
                "git spec paths must be non-blank strings",
                code="invalid_git_paths",
                cause=f"path entry {p!r} is blank or not a string",
                remediation="Give each path a non-empty absolute path inside the workspace.",
            )

    if credentials_secret is not None and (
        not isinstance(credentials_secret, (list, tuple))
        or len(credentials_secret) != 2
        or not all(isinstance(x, str) and x.strip() for x in credentials_secret)
    ):
        raise AgentRunError(
            "git spec credentials_secret must be a (secret_name, secret_key) pair",
            code="invalid_git_credentials",
            cause=f"credentials_secret {credentials_secret!r} is not a 2-tuple of non-blank strings",
            remediation='Pass credentials_secret=("my-secret", "token") naming the Secret and key.',
        )

    if credentials_username is not None and credentials_secret is None:
        raise AgentRunError(
            "git spec credentials_username needs credentials_secret",
            code="invalid_git_credentials",
            cause="credentials_username was set without credentials_secret",
            remediation="Pass credentials_secret=(name, key) alongside credentials_username, or drop the username.",
        )


def git(
    paths: list[str],
    *,
    credentials_secret: Optional[tuple[str, str]] = None,
    credentials_username: Optional[str] = None,
) -> GitSpec:
    """Build a validated :class:`GitSpec` for a Workspace ``spec.git``.

    See :class:`GitSpec` for the field meanings. Validation runs in
    :meth:`GitSpec.__post_init__` and fails fast with an LLM-legible
    :class:`~mitos.errors.AgentRunError` so a malformed declaration is caught
    before it reaches the cluster:

    - ``invalid_git_paths`` if ``paths`` is empty or has a blank entry.
    - ``invalid_git_credentials`` if ``credentials_secret`` is not a
      ``(name, key)`` pair of non-blank strings, or ``credentials_username`` is
      set without ``credentials_secret``.
    """
    return GitSpec(
        paths=paths,
        credentials_secret=credentials_secret,
        credentials_username=credentials_username,
    )


def coerce_git(git_arg: object) -> GitSpec:
    """Normalise a ``git=`` argument into a :class:`GitSpec`.

    Accepts a :class:`GitSpec`, or a bare list/tuple of paths as a convenience
    for the common paths-only case. Any other value raises ``invalid_git_spec``.
    """
    if isinstance(git_arg, GitSpec):
        return git_arg
    if isinstance(git_arg, (list, tuple)):
        return git(paths=list(git_arg))
    raise AgentRunError(
        "git= must be a GitSpec or a list of paths",
        code="invalid_git_spec",
        cause=f"got {type(git_arg).__name__}",
        remediation="Pass git=mitos.git(paths=[...]) or git=[\"/workspace/repo\"].",
    )
