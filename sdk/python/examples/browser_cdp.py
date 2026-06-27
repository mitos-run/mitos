"""Computer-use example: drive a headless Chromium sandbox over CDP.

Prerequisites:
  - A sandbox-server (cmd/sandbox-server) running against the computer-use
    image rootfs (ghcr.io/mitos-run/mitos-computer-use), so a forked sandbox
    boots Chromium listening on the Chrome DevTools Protocol (CDP) port 9222.
    On a Kubernetes cluster this is a SandboxPool whose template sets the
    workload and ~1.5 to 2 GiB of memory; see
    docs/recipes/browser-automation.md.
  - The Playwright client: ``pip install playwright`` (no browser download is
    needed; this attaches to the remote Chromium over CDP).

Run::

    python3 browser_cdp.py [base_url]

It creates a warm browser sandbox (Chromium snapshotted already listening, so the
fork wakes warm), opens a CDP endpoint to it, then connects Playwright, navigates,
clicks, screenshots, and prints "browser_cdp example OK". The base URL comes from
argv[1], else MITOS_BASE_URL, else http://localhost:8080.

This uses the loopback host-forward to reach CDP, which works against any
standalone sandbox-server. For an authenticated named URL over the preview proxy,
use ``sandbox.get_host(9222)`` instead (see the recipe).
"""

import os
import sys

import httpx

import mitos


def cdp_forward_url(base_url: str, sandbox_id: str) -> str:
    """Open a loopback host-forward to the sandbox CDP port and return the
    http://host:port a CDP client connects to. Chromium reflects the request
    Host into webSocketDebuggerUrl, so connect_over_cdp works against it."""
    resp = httpx.post(
        f"{base_url}/v1/sandboxes/{sandbox_id}/forward",
        json={"guest_port": 9222},
        timeout=30,
    )
    resp.raise_for_status()
    return "http://" + resp.json()["host"]


def main(base_url: str) -> None:
    from playwright.sync_api import sync_playwright

    # Warm browser sandbox: the template runs Chromium as a serving workload and
    # snapshots it already listening on CDP, so the fork wakes warm. Chromium
    # needs more than the 512 MiB default.
    sandbox = mitos.create(
        "browser",
        base_url=base_url,
        workload={
            "command": ["/usr/local/bin/start-chromium.sh"],
            "ready": {"port": 9222, "path": "/json/version", "expect": 200,
                      "timeout_seconds": 90},
        },
        resources={"vcpu_count": 2, "mem_size_mib": 1536},
    )
    try:
        endpoint = cdp_forward_url(base_url, sandbox.id)
        with sync_playwright() as p:
            browser = p.chromium.connect_over_cdp(endpoint)
            context = browser.contexts[0] if browser.contexts else browser.new_context()
            page = context.new_page()
            page.goto(
                "data:text/html,<title>mitos</title>"
                "<button id=go>go</button><div id=out>idle</div>"
                "<script>go.onclick=()=>out.textContent='clicked'</script>"
            )
            page.click("#go")
            title = page.title()
            outcome = page.inner_text("#out")
            page.screenshot(path="browser_cdp.png")
            browser.close()
        if title != "mitos" or outcome != "clicked":
            raise SystemExit(f"unexpected result: title={title!r} out={outcome!r}")
        print(f"navigated, title={title!r}, click result={outcome!r}, wrote browser_cdp.png")
        print("browser_cdp example OK")
    finally:
        sandbox.terminate()


if __name__ == "__main__":
    url = sys.argv[1] if len(sys.argv) > 1 else os.environ.get(
        "MITOS_BASE_URL", "http://localhost:8080"
    )
    main(url)
