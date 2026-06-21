"""Code-first template builder (issue #220).

``Template`` is a fluent builder that authors a ``SandboxTemplate`` spec from
code, matching E2B's ``Template().from_image(...).copy(...).run_cmd(...)`` and
Daytona's declarative Builder. It maps onto the mitos ``SandboxTemplate`` CRD:
the base image, an ordered list of build steps (copy / run / env / workdir),
the start command, and resources.

The builder is pure: it constructs a dict, it does not talk to a server or boot
a VM. Hand the emitted spec to the CLI (``mitos template build --spec``) or apply
the wrapped object (``to_template``) to a cluster; the node then builds the
snapshot. The ordered step list feeds the chained, content-addressed build cache
on the build side, so an unchanged prefix is reused.

Example::

    spec = (
        Template()
        .from_image("python:3.12")
        .copy("app/", "/app")
        .run("pip install -r requirements.txt")
        .set_start("python app.py")
        .to_spec()
    )
"""

from __future__ import annotations

import shlex
from typing import Dict, List, Optional, Union


class Template:
    """Fluent builder for a SandboxTemplate spec.

    Every step method returns ``self`` so calls chain. ``to_spec`` emits the
    spec dict; ``to_template`` wraps it in a full SandboxTemplate object with a
    name.
    """

    def __init__(self) -> None:
        self._image: Optional[str] = None
        self._steps: List[Dict[str, str]] = []
        self._command: List[str] = []
        self._cpu: Optional[str] = None
        self._memory: Optional[str] = None

    def from_image(self, image: str) -> "Template":
        """Set the base OCI image (e.g. ``python:3.12``)."""
        self._image = image
        return self

    def run(self, command: str) -> "Template":
        """Add a build-time command, run inside the booting template VM. Mirrors
        E2B ``run_cmd`` and Dockerfile ``RUN``."""
        self._steps.append({"type": "run", "run": command})
        return self

    # run_cmd is the E2B spelling; keep it as an alias for familiarity.
    run_cmd = run

    def env(self, name: str, value: str) -> "Template":
        """Bake an environment variable into the template (Dockerfile ``ENV``).
        The value is part of the build cache key."""
        self._steps.append({"type": "env", "envName": name, "envValue": value})
        return self

    def workdir(self, path: str) -> "Template":
        """Set the working directory for the remaining steps (Dockerfile
        ``WORKDIR``)."""
        self._steps.append({"type": "workdir", "workdir": path})
        return self

    def copy(self, source: str, dest: str) -> "Template":
        """Stage host files into the image (Dockerfile ``COPY``). ``source`` is
        the host path, ``dest`` the in-image path. The file materialization runs
        on the build node; the builder records the declared paths so the cache
        key chains over them."""
        self._steps.append({"type": "copy", "source": source, "dest": dest})
        return self

    def set_start(self, command: Union[str, List[str]]) -> "Template":
        """Set the template start command. A string is split with shell rules; a
        list is taken as the argv verbatim."""
        if isinstance(command, str):
            self._command = shlex.split(command)
        else:
            self._command = list(command)
        return self

    def cpu(self, cpu: str) -> "Template":
        """Set the CPU request (a Kubernetes quantity, e.g. ``"2"``)."""
        self._cpu = cpu
        return self

    def memory(self, memory: str) -> "Template":
        """Set the memory request (a Kubernetes quantity, e.g. ``"1Gi"``)."""
        self._memory = memory
        return self

    def to_spec(self) -> Dict[str, object]:
        """Emit the SandboxTemplate spec dict. Raises ``ValueError`` if no base
        image was set (a template must start from an image)."""
        if not self._image:
            raise ValueError(
                "a base image is required: call .from_image(...) before building the template"
            )
        spec: Dict[str, object] = {"image": self._image}
        if self._steps:
            spec["buildSteps"] = [dict(s) for s in self._steps]
        if self._command:
            spec["command"] = list(self._command)
        resources: Dict[str, str] = {}
        if self._cpu:
            resources["cpu"] = self._cpu
        if self._memory:
            resources["memory"] = self._memory
        if resources:
            spec["resources"] = resources
        return spec

    def to_template(self, name: str) -> Dict[str, object]:
        """Wrap the spec in a full SandboxTemplate object with ``metadata.name``
        set, ready to apply to a cluster or write to a YAML file."""
        return {
            "apiVersion": "mitos.run/v1alpha1",
            "kind": "SandboxTemplate",
            "metadata": {"name": name},
            "spec": self.to_spec(),
        }
