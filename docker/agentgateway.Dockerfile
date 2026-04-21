FROM ubuntu:24.04

# Install Node.js, Python, uv, and dependencies
RUN apt-get update && apt-get install -y \
    curl \
    ca-certificates \
    gnupg \
    python3 \
    python3-pip \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_20.x nodistro main" | tee /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y nodejs \
    && curl -LsSf https://astral.sh/uv/install.sh | sh \
    && mv /root/.local/bin/uv /usr/local/bin/uv \
    && mv /root/.local/bin/uvx /usr/local/bin/uvx \
    && rm -rf /var/lib/apt/lists/*

# Download agentgateway v1.1.0 binary (not available as Docker image; only as GitHub release)
ARG TARGETARCH
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) AG_ARCH="amd64" ;; \
        arm64) AG_ARCH="arm64" ;; \
        *) echo "Unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/agentgateway/agentgateway/releases/download/v1.1.0/agentgateway-linux-${AG_ARCH}" \
        -o /usr/local/bin/agentgateway; \
    chmod +x /usr/local/bin/agentgateway; \
    /usr/local/bin/agentgateway --version

WORKDIR /app

LABEL org.opencontainers.image.source=https://github.com/agentregistry-dev/agentregistry
LABEL org.opencontainers.image.description="Agent Registry Agent Gateway with NPX and UVX built in"
LABEL org.opencontainers.image.authors="Agent Registry Creators 🤖"

ENTRYPOINT ["/usr/local/bin/agentgateway"]
# The config file will be mounted via volume
CMD ["-f", "/config/agent-gateway.yaml"]
