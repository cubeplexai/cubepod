# Interactive Desktop & Browser GUI Sandbox Image (`cubepod-desktop`)
# Conforms to CEP-0021: Interactive Desktop & Browser GUI Stack with Lazy Activation
#
# Build from repository root:
#   docker build -f images/desktop/Dockerfile -t cubepod-desktop:latest .
#
# Note: `podd-init` and `podd` are NEVER baked into sandbox images (Core Invariant #2).
# They are injected at Pod startup into `/opt/cubepod/bin` via the `cubepod-podd`
# initContainer (or volume mount in local Docker). All desktop supervisor tooling lives
# under `/opt/cubepod/desktop/` so the `/opt/cubepod/bin` emptyDir mount never shadows it.
#
# Nothing GUI-related starts on boot; the container remains idle (~0 MiB GUI overhead)
# until `/opt/cubepod/desktop/cubepod-desktop-ctl start` is invoked.
# WebRTC & Fallback: `/usr/local/bin/neko` and `/opt/cubepod/desktop/neko-www` are packaged
# from the official multi-arch Neko base image (`ghcr.io/m1k1o/neko/base`). Requesting
# `--transport webrtc` launches Neko alongside `noVNC` (`:8081` + `:8080`), falling back
# cleanly to `noVNC` if Neko is absent or fails to start.

ARG NEKO_IMAGE=ghcr.io/m1k1o/neko/base:latest
FROM ${NEKO_IMAGE} AS neko

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/cubepod/desktop/ms-playwright

RUN apt-get update && apt-get install -y --no-install-recommends \
    # Core utilities
    bash ca-certificates curl e2fsprogs util-linux iproute2 git procps socat \
    python3 python3-pip python3-venv libatomic1 \
    # X11, window manager, VNC, noVNC/websockify, terminal, and fonts
    xvfb openbox x11vnc novnc websockify dbus-x11 xauth ttyd \
    fonts-liberation fonts-noto-cjk \
    # Neko WebRTC GStreamer video/audio runtime & X11 capture/input libraries
    libgtk-3-0t64 libxtst6 libxcvt0 pulseaudio \
    libgstreamer1.0-0 libgstreamer-plugins-base1.0-0 \
    gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
    gstreamer1.0-x gstreamer1.0-pulseaudio \
    libopus0 libvpx9 \
    && rm -rf /var/lib/apt/lists/* \
    # Python scientific/code-interpreter + Playwright browser automation packages
    && pip install --no-cache-dir --break-system-packages --ignore-installed \
       ipykernel matplotlib pandas numpy playwright \
    # Install Playwright Chromium & OS shared library dependencies (non-snap)
    && mkdir -p /opt/cubepod/desktop/ms-playwright \
    && playwright install chromium \
    && playwright install-deps chromium \
    && rm -rf /var/lib/apt/lists/* /root/.cache /tmp/* \
    # Expose stable symlink to Playwright Chromium binary
    && CHROME_BIN="$(find /opt/cubepod/desktop/ms-playwright -path '*/chromium-*/chrome-linux*/chrome' -type f | head -n 1)" \
    && test -n "$CHROME_BIN" \
    && ln -sf "$CHROME_BIN" /opt/cubepod/desktop/chromium-bin \
    && chmod -R a+rX /opt/cubepod/desktop/ms-playwright \
    # Use existing UID/GID 1000 user (e.g. `ubuntu` in ubuntu:24.04) or create `sandbox` if absent
    && TARGET_USER="$(id -nu 1000 2>/dev/null || true)" \
    && if [ -z "$TARGET_USER" ]; then \
         groupadd -g 1000 sandbox \
         && useradd -m -u 1000 -g 1000 -s /bin/bash sandbox \
         && TARGET_USER=sandbox; \
       fi \
    && TARGET_HOME="$(getent passwd 1000 | cut -d: -f6)" \
    && usermod -aG audio,video "$TARGET_USER" \
    && mkdir -p /workspace "${TARGET_HOME}/.config/chromium" /run/cubepod/desktop /tmp/.X11-unix \
    && chown -R 1000:1000 /workspace "${TARGET_HOME}" /run/cubepod/desktop \
    && chmod 1777 /tmp/.X11-unix

COPY --from=neko /usr/bin/neko /usr/local/bin/neko
COPY --from=neko /var/www /opt/cubepod/desktop/neko-www

COPY images/desktop/cubepod-desktop-ctl /opt/cubepod/desktop/cubepod-desktop-ctl
COPY images/desktop/chromium-wrapper /opt/cubepod/desktop/chromium-wrapper
COPY images/desktop/browser_tab_follow.py /opt/cubepod/desktop/browser_tab_follow.py

RUN chmod 0755 \
      /usr/local/bin/neko \
      /opt/cubepod/desktop/cubepod-desktop-ctl \
      /opt/cubepod/desktop/chromium-wrapper \
      /opt/cubepod/desktop/browser_tab_follow.py \
    && chmod -R a+rX /opt/cubepod/desktop/neko-www \
    && ln -sf /opt/cubepod/desktop/cubepod-desktop-ctl /usr/local/bin/cubepod-desktop-ctl \
    && ln -sf /opt/cubepod/desktop/chromium-wrapper /usr/local/bin/chromium \
    && ln -sf /opt/cubepod/desktop/chromium-wrapper /usr/local/bin/chromium-browser \
    && ln -sf /opt/cubepod/desktop/browser_tab_follow.py /usr/local/bin/cubeplex-focus-browser-tab \
    && ln -sf /opt/cubepod/desktop/browser_tab_follow.py /usr/local/bin/cubepod-focus-browser-tab

WORKDIR /workspace

CMD ["sleep", "infinity"]
