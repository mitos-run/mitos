# Chromium Computer-Use Template

This OCI image provides a headless Chromium browser with Chrome DevTools Protocol (CDP) support for browser-automation workloads in Mitos sandboxes.

## Features

- Headless Chromium with CDP endpoint on `127.0.0.1:9222`
- Pre-launched on container startup via `start-chromium.sh`
- Includes fonts and CA certificates for web rendering
- Built on `debian:stable-slim` with `setsid` support for the serving-workload path

## Design

The image runs Chromium with `--no-sandbox` because the Firecracker microVM is the isolation boundary; Chrome's own sandbox is redundant and unnecessary. See `docs/threat-model.md` for the threat model.

## Usage

This image is designed to run as a Mitos serving workload, captured in a template snapshot and forked warm on demand. The guest exposes port 9222 via the Mitos expose proxy (authenticated, never public):

```bash
mitos workspace serve <workspace> --pool <pool> --expose 9222
```

Access the CDP endpoint via the authenticated expose URL at `/json/version`.

For end-to-end browser automation examples, see the recipe at `../../docs/recipes/browser-automation.md`.
