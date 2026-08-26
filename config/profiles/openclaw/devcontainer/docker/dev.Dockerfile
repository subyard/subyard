# Copyable reference dev image — `openclaw` profile (Node + Python).
#
# Genericized from a proven OpenClaw devcontainer. Public/committed: no secrets,
# no host paths, no private naming. Carries only the OS toolchain — project tools
# (vitest/typescript/@types, Python tools) come from the project's vendored deps
# per config/profiles/openclaw/profile.conf, so a version bump has a single source of
# truth in the project repo. Optional heavy features (browser_tests,
# sandbox_tests) are off by default — see that profile.
FROM ubuntu:24.04

# OS toolchain pins — keep in sync with config/profiles/openclaw/profile.conf.
ARG NODE_VERSION=24.15.0
ARG COREPACK_VERSION=0.31.0
ARG PNPM_VERSION=11.2.2

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
  bash \
  bubblewrap \
  build-essential \
  ca-certificates \
  curl \
  dnsutils \
  git \
  iproute2 \
  jq \
  less \
  lsof \
  openssh-client \
  procps \
  python3 \
  python3-pip \
  python3-venv \
  ripgrep \
  shellcheck \
  unzip \
  xz-utils \
  && rm -rf /var/lib/apt/lists/*

# Node.js from the official dist tarball, checksum-verified.
RUN set -eux; \
  arch="$(dpkg --print-architecture)"; \
  case "$arch" in \
    amd64) node_arch="x64" ;; \
    arm64) node_arch="arm64" ;; \
    *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
  esac; \
  node_dist="node-v${NODE_VERSION}-linux-${node_arch}"; \
  curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/${node_dist}.tar.xz"; \
  curl -fsSLO "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt"; \
  grep "  ${node_dist}.tar.xz$" SHASUMS256.txt | sha256sum -c -; \
  tar -xJf "${node_dist}.tar.xz" -C /usr/local --strip-components=1 --no-same-owner; \
  rm "${node_dist}.tar.xz" SHASUMS256.txt; \
  node -e "const major = process.versions.node.split('.')[0]; if (major !== '24') { throw new Error('expected Node.js major 24, got ' + process.versions.node); }"; \
  node --version; \
  npm --version

RUN npm install -g "corepack@${COREPACK_VERSION}" \
  && corepack enable \
  && corepack prepare "pnpm@${PNPM_VERSION}" --activate \
  && corepack pnpm --version

# Arch-scope pnpm so ad hoc installs never fetch Windows/macOS or musl variants
# of optional native packages — the devcontainer is always linux/glibc.
RUN printf '%s\n' \
    '#!/usr/bin/env sh' \
    'exec /usr/local/bin/corepack pnpm \' \
    '  --config.supportedArchitectures.os=linux \' \
    '  --config.supportedArchitectures.cpu=current \' \
    '  --config.supportedArchitectures.libc=glibc \' \
    '  "$@"' \
    > /usr/local/bin/pnpm \
  && chmod 0755 /usr/local/bin/pnpm \
  && pnpm --version

# Empty project venv; the project installs its own Python tools into it
# (pyproject is the source of truth — see profile.conf).
RUN python3 -m venv /opt/venv \
  && /opt/venv/bin/python -m pip install --upgrade pip setuptools wheel

# Normalize uid/gid 1000 to the `dev` user (matches DEV_USER/DEV_UID/DEV_GID).
RUN set -eux; \
  if getent group 1000 >/dev/null; then \
    existing_group="$(getent group 1000 | cut -d: -f1)"; \
    [ "$existing_group" = dev ] || groupmod --new-name dev "$existing_group"; \
  else \
    groupadd --gid 1000 dev; \
  fi; \
  if getent passwd 1000 >/dev/null; then \
    existing_user="$(getent passwd 1000 | cut -d: -f1)"; \
    [ "$existing_user" = dev ] || usermod --login dev --home /home/dev --move-home "$existing_user"; \
    usermod --gid 1000 --shell /bin/bash dev; \
  else \
    useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash dev; \
  fi; \
  mkdir -p /workspace /home/dev/.cache/pip /home/dev/.cache/node /home/dev/.npm; \
  chown -R dev:dev /home/dev /workspace; \
  chmod g+rwx /home/dev; \
  chmod -R g+rwX /home/dev/.cache /home/dev/.npm

ENV HOME=/home/dev
ENV VIRTUAL_ENV=/opt/venv
ENV PIP_CACHE_DIR=/workspace/.cache/pip
ENV NPM_CONFIG_CACHE=/workspace/.cache/npm
ENV COREPACK_HOME=/workspace/.cache/node/corepack
ENV XDG_DATA_HOME=/workspace/.cache/xdg
ENV PNPM_HOME=/workspace/.cache/pnpm/home
ENV NO_UPDATE_NOTIFIER=1
ENV npm_config_fetch_timeout=900000
ENV npm_config_fetch_retries=8
ENV npm_config_fetch_retry_maxtimeout=300000
ENV PATH="/home/dev/.local/bin:/opt/venv/bin:${PATH}"

WORKDIR /workspace
CMD ["sleep", "infinity"]
