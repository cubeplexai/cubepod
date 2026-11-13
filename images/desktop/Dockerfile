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

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/cubepod/desktop/ms-playwright

RUN apt-get update && apt-get install -y --no-install-recommends \
    # Core utilities
    bash ca-certificates curl e2fsprogs util-linux iproute2 git procps socat \
    python3 python3-pip python3-venv \
    # X11, window manager, VNC, noVNC/websockify, terminal, and fonts
    xvfb openbox x11vnc novnc websockify dbus-x11 xauth ttyd \
    fonts-liberation fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/* \
    # Python scientific/code-interpreter + Playwright browser automation packages
    && pip install --no-cache-dir --break-system-packages \
       ipykernel matplotlib pandas numpy playwright \
    # Install Playwright Chromium & OS shared library dependencies (non-snap)
    && mkdir -p /opt/cubepod/desktop/ms-playwright \
    && playwright install chromium \
    && playwright install-deps chromium \
    && rm -rf /var/lib/apt/lists/* \
    # Expose stable symlink to Playwright Chromium binary
    && CHROME_BIN="$(find /opt/cubepod/desktop/ms-playwright -path '*/chromium-*/chrome-linux*/chrome' -type f | head -n 1)" \
    && test -n "$CHROME_BIN" \
    && ln -sf "$CHROME_BIN" /opt/cubepod/desktop/chromium-bin \
    && chmod -R a+rX /opt/cubepod/desktop/ms-playwright \
    # Ensure UID/GID 1000 is cleanly assigned to unprivileged user `cubeplex`
    && if id -u 1000 >/dev/null 2>&1; then userdel -r "$(id -nu 1000)"; fi \
    && if getent group 1000 >/dev/null 2>&1; then groupdel "$(getent group 1000 | cut -d: -f1)"; fi \
    && groupadd -g 1000 cubeplex \
    && useradd -m -u 1000 -g 1000 -s /bin/bash cubeplex \
    && mkdir -p /workspace /home/cubeplex/.config/chromium /run/cubepod/desktop /tmp/.X11-unix \
    && chown -R 1000:1000 /workspace /home/cubeplex /run/cubepod/desktop \
    && chmod 1777 /tmp/.X11-unix

COPY images/desktop/cubepod-desktop-ctl /opt/cubepod/desktop/cubepod-desktop-ctl
COPY images/desktop/chromium-wrapper /opt/cubepod/desktop/chromium-wrapper

RUN chmod 0755 /opt/cubepod/desktop/cubepod-desktop-ctl /opt/cubepod/desktop/chromium-wrapper \
    && ln -sf /opt/cubepod/desktop/cubepod-desktop-ctl /usr/local/bin/cubepod-desktop-ctl \
    && ln -sf /opt/cubepod/desktop/chromium-wrapper /usr/local/bin/chromium \
    && ln -sf /opt/cubepod/desktop/chromium-wrapper /usr/local/bin/chromium-browser

WORKDIR /workspace

CMD ["sleep", "infinity"]
