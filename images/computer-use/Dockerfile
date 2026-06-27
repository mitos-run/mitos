# Headless Chromium + CDP template for Mitos browser-automation sandboxes (#314).
# The microVM is the isolation boundary; Chromium runs with --no-sandbox.
FROM debian:stable-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium \
      fonts-liberation \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY start-chromium.sh /usr/local/bin/start-chromium.sh
RUN chmod +x /usr/local/bin/start-chromium.sh

# CDP endpoint. Reach it over the Mitos expose proxy, never publicly.
EXPOSE 9222
