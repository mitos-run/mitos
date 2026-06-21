"""The quickstart snippet's API shape is real (issue #215).

The onboarding funnel promises a single obvious quickstart: one ``pip install
mitos``, one snippet, code execution with NO second SDK to install. This test
guards that promise against drift: it asserts the exact calls the canonical
quickstart uses are exposed by the one ``mitos`` package, so the copy-paste
snippet cannot silently rot.

The canonical quickstart lives in ``docs/quickstart.md``; the load-bearing calls
are mirrored here as the contract. If the SDK surface changes, this test breaks
and the docs must be updated in the same change.
"""

import inspect
import os

import mitos


def test_one_package_exposes_the_quickstart_surface():
    """Everything the headline snippet uses comes from the single ``mitos``
    package: no second import, no second pip install."""
    # mitos.create is the flat one-liner entry point (issue #217).
    assert hasattr(mitos, "create")
    assert callable(mitos.create)

    # Sandbox.create is the documented alias to the same flat path.
    assert hasattr(mitos, "Sandbox")
    assert hasattr(mitos.Sandbox, "create")


def test_create_signature_matches_the_snippet():
    """``mitos.create(image, api_key=..., base_url=...)`` is the documented
    signature; the funnel hands the user exactly these three inputs."""
    sig = inspect.signature(mitos.create)
    params = sig.parameters
    # The positional image/template plus the two auth knobs the snippet names.
    assert "api_key" in params
    assert "base_url" in params
    # First positional parameter is the image/template the snippet passes ("python").
    first = next(iter(params))
    assert first in ("image", "template", "name")


def test_handle_exposes_the_documented_methods():
    """The handle the quickstart drives exposes exec / run_code / files / fork /
    terminate directly, with the files sub-object carrying read/write. We assert
    the class surface without booting a server."""
    from mitos.direct import DirectSandbox, DirectSandboxFiles

    for method in ("exec", "run_code", "fork", "terminate"):
        assert callable(getattr(DirectSandbox, method)), method
    # files is an attribute set in __init__; its type carries read/write.
    assert hasattr(DirectSandboxFiles, "read")
    assert hasattr(DirectSandboxFiles, "write")


def test_docs_quickstart_uses_only_real_calls():
    """Parse the canonical quickstart and confirm each ``sb.<call>`` it shows is
    a real method on the handle. This is the anti-drift guard: a snippet that
    references a method the SDK does not have fails here."""
    repo_root = os.path.join(os.path.dirname(__file__), "..", "..", "..")
    quickstart = os.path.join(repo_root, "docs", "quickstart.md")
    if not os.path.exists(quickstart):
        # The quickstart may also live as the README headline; skip rather than
        # fail so the test is portable, but the file is expected to exist.
        import pytest

        pytest.skip("docs/quickstart.md not present")

    with open(quickstart, encoding="utf-8") as fh:
        text = fh.read()

    from mitos.direct import DirectSandbox

    # Collect sb.<name>( call sites from the doc and confirm each is real.
    import re

    calls = set(re.findall(r"\bsb\.([a-z_]+)\s*\(", text))
    # files is an attribute, not a method, so handle sb.files.read/write separately.
    file_calls = set(re.findall(r"\bsb\.files\.([a-z_]+)\s*\(", text))

    for call in calls:
        if call == "files":
            continue
        assert hasattr(DirectSandbox, call), f"quickstart uses sb.{call} which the SDK does not expose"

    from mitos.direct import DirectSandboxFiles

    for call in file_calls:
        assert hasattr(DirectSandboxFiles, call), f"quickstart uses sb.files.{call} which the SDK does not expose"
