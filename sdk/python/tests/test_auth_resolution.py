"""Unit tests for the unified bearer-token resolution (no server required).

The flat SDK resolves the bearer with this precedence: explicit ``api_key``
argument, then ``MITOS_API_KEY``, then the CLI login credential file written by
``mitos auth login`` (MITOS_CONFIG_DIR else ~/.config/mitos/credentials.json,
the ``token`` field), then None (tokenless). A missing, unreadable, or
non-JSON file is never an error; it simply yields no token. The token VALUE is
never logged.
"""

import json
import os

import pytest

from mitos.direct import _resolve_auth


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch):
    """Each test starts with no MITOS_* env so precedence is deterministic."""
    monkeypatch.delenv("MITOS_API_KEY", raising=False)
    monkeypatch.delenv("MITOS_CONFIG_DIR", raising=False)
    monkeypatch.delenv("MITOS_BASE_URL", raising=False)


def _write_credentials(dir_path, token):
    with open(os.path.join(dir_path, "credentials.json"), "w") as f:
        json.dump({"token": token, "email": "a@b.c", "default_org": "org-1"}, f)


def test_credential_file_used_when_env_unset(tmp_path, monkeypatch):
    _write_credentials(str(tmp_path), "file-tok")
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    key, _ = _resolve_auth(None, None)
    assert key == "file-tok"


def test_env_overrides_credential_file(tmp_path, monkeypatch):
    _write_credentials(str(tmp_path), "file-tok")
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    monkeypatch.setenv("MITOS_API_KEY", "env-tok")
    key, _ = _resolve_auth(None, None)
    assert key == "env-tok"


def test_explicit_arg_overrides_everything(tmp_path, monkeypatch):
    _write_credentials(str(tmp_path), "file-tok")
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    monkeypatch.setenv("MITOS_API_KEY", "env-tok")
    key, _ = _resolve_auth("arg-tok", None)
    assert key == "arg-tok"


def test_no_file_no_env_is_tokenless(tmp_path, monkeypatch):
    # Empty config dir: no credentials.json present.
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    key, _ = _resolve_auth(None, None)
    assert key is None


def test_unreadable_or_invalid_json_is_not_an_error(tmp_path, monkeypatch):
    with open(os.path.join(str(tmp_path), "credentials.json"), "w") as f:
        f.write("{ this is not valid json")
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    key, _ = _resolve_auth(None, None)
    assert key is None


def test_file_without_token_field_is_tokenless(tmp_path, monkeypatch):
    with open(os.path.join(str(tmp_path), "credentials.json"), "w") as f:
        json.dump({"email": "a@b.c"}, f)
    monkeypatch.setenv("MITOS_CONFIG_DIR", str(tmp_path))
    key, _ = _resolve_auth(None, None)
    assert key is None
