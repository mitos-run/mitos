"""Tests for the code-first Template builder (issue #220).

The builder is a fluent API that emits a SandboxTemplate spec: a base image, an
ordered list of build steps (copy / run / env / workdir), a start command, and
resources. It needs no server and no KVM: it is pure spec construction.
"""

from mitos.template import Template


def test_builder_emits_expected_spec():
    spec = (
        Template()
        .from_image("python:3.12")
        .workdir("/app")
        .copy("app/", "/app")
        .env("PORT", "8080")
        .run("pip install -r requirements.txt")
        .set_start("python app.py")
        .cpu("2")
        .memory("1Gi")
        .to_spec()
    )

    assert spec["image"] == "python:3.12"
    steps = spec["buildSteps"]
    assert [s["type"] for s in steps] == ["workdir", "copy", "env", "run"]
    assert steps[0] == {"type": "workdir", "workdir": "/app"}
    assert steps[1] == {"type": "copy", "source": "app/", "dest": "/app"}
    assert steps[2] == {"type": "env", "envName": "PORT", "envValue": "8080"}
    assert steps[3] == {"type": "run", "run": "pip install -r requirements.txt"}
    # set_start maps to the template start command.
    assert spec["command"] == ["python", "app.py"]
    assert spec["resources"]["cpu"] == "2"
    assert spec["resources"]["memory"] == "1Gi"


def test_builder_requires_base_image():
    import pytest

    with pytest.raises(ValueError):
        Template().run("echo hi").to_spec()


def test_builder_is_fluent_and_ordered():
    """Steps are recorded in call order, so the chained cache key is stable."""
    spec = (
        Template()
        .from_image("node:24")
        .run("npm ci")
        .run("npm run build")
        .to_spec()
    )
    assert [s["run"] for s in spec["buildSteps"]] == ["npm ci", "npm run build"]


def test_set_start_accepts_list_form():
    spec = Template().from_image("alpine").set_start(["/bin/sh", "-c", "sleep 1"]).to_spec()
    assert spec["command"] == ["/bin/sh", "-c", "sleep 1"]


def test_empty_build_omits_buildsteps_key():
    spec = Template().from_image("busybox").to_spec()
    assert "buildSteps" not in spec
    assert spec["image"] == "busybox"


def test_to_template_wraps_spec_with_name():
    obj = Template().from_image("busybox").to_template("my-tmpl")
    assert obj["apiVersion"] == "mitos.run/v1"
    assert obj["kind"] == "SandboxPool"
    assert obj["metadata"]["name"] == "my-tmpl"
    assert obj["spec"]["template"]["image"] == "busybox"
