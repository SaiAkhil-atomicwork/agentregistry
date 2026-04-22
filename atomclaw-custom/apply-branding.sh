#!/bin/bash
#
# AtomClaw branding for AgentRegistry + AgentGateway
#
# REPEATABLE: Safe to run multiple times. Safe to run after upstream merges.
# Usage: bash atomclaw-custom/apply-branding.sh
#
# What this changes:
#   - Registry UI (Next.js, owned by us in this repo):
#       * Wordmark/logo in nav -> Atom icon + "toolregistry"
#       * Tab title metadata -> "ToolRegistry"
#       * Footer (with Solo.io credit + GitHub/Discord links) -> removed
#       * Favicon (/icon.svg) -> Atom icon
#   - AgentGateway admin UI (upstream binary, we don't own the source):
#       * nginx vhost installed at /etc/nginx/sites-available/atomclaw-toolgateway
#         that uses sub_filter to swap "agentgateway" -> "toolgateway" and
#         injects our atom favicon. Also publishes the atom icon under
#         /var/www/atomclaw/.
#
# What this does NOT change:
#   - upstream agentgateway Go binary (rebrand is via nginx response rewrites)
#   - the agentregistry Go module name (functional)
#
# After running this on EC2-B you must rebuild + restart:
#   make build-ui
#   docker build -f docker/server.Dockerfile -t $REG/server:$VERSION .
#   docker push $REG/server:$VERSION
#   docker restart agentregistry-server
#   sudo systemctl reload nginx

set -e

BRAND="AtomClaw"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
UI_DIR="$REPO_DIR/ui"
ASSETS_DIR="$SCRIPT_DIR/assets"
NGINX_SNIPPET="$SCRIPT_DIR/nginx/toolgateway.conf"

echo "═══════════════════════════════════════════"
echo "  Applying $BRAND branding"
echo "═══════════════════════════════════════════"
echo "Repo:   $REPO_DIR"
echo "UI:     $UI_DIR"
echo "Assets: $ASSETS_DIR"
echo

# ──────────────────────────────────────────────────────────────────────────
# 1. Static assets — copy atom icon into ui/public/ (favicon + nav logo)
# ──────────────────────────────────────────────────────────────────────────
echo "→ Copying atom-icon.svg into ui/public/"
cp "$ASSETS_DIR/atom-icon.svg" "$UI_DIR/public/atom-icon.svg"
cp "$ASSETS_DIR/atom-icon.svg" "$UI_DIR/public/icon.svg"
echo "  ✓ ui/public/atom-icon.svg"
echo "  ✓ ui/public/icon.svg (favicon)"

# ──────────────────────────────────────────────────────────────────────────
# 2. Source patches — verify the rebrand patches are present in source.
#    Fail loudly if upstream merged something that clobbered them.
# ──────────────────────────────────────────────────────────────────────────
echo
echo "→ Verifying source patches"

LAYOUT="$UI_DIR/app/layout.tsx"
NAV="$UI_DIR/components/navigation.tsx"

check_patch() {
    local file="$1"
    local needle="$2"
    local label="$3"
    if grep -qF "$needle" "$file"; then
        echo "  ✓ $label"
    else
        echo "  ✗ MISSING in $file: $label"
        echo "    Look for: $needle"
        exit 1
    fi
}

check_patch "$LAYOUT" 'title: "ToolRegistry"' "layout.tsx → metadata title"
check_patch "$NAV"    "src=\"/atom-icon.svg\""  "navigation.tsx → atom-icon.svg referenced"
check_patch "$NAV"    ">toolregistry<"          "navigation.tsx → toolregistry wordmark"

if grep -qF "<Footer />" "$LAYOUT"; then
    echo "  ✗ layout.tsx still renders <Footer /> — Solo.io credit not removed"
    exit 1
else
    echo "  ✓ layout.tsx → <Footer /> removed"
fi

# ──────────────────────────────────────────────────────────────────────────
# 3. AgentGateway nginx rebrand — install vhost + serve atom icon
#    (only when running on the EC2 host as root; skipped otherwise)
# ──────────────────────────────────────────────────────────────────────────
echo
echo "→ AgentGateway nginx rebrand"

if [[ -d /etc/nginx/sites-available && $(id -u) -eq 0 ]]; then
    install -d /var/www/atomclaw
    install -m 0644 "$ASSETS_DIR/atom-icon.svg" /var/www/atomclaw/atom-icon.svg
    install -m 0644 "$NGINX_SNIPPET" /etc/nginx/sites-available/atomclaw-toolgateway
    ln -sf /etc/nginx/sites-available/atomclaw-toolgateway /etc/nginx/sites-enabled/atomclaw-toolgateway
    echo "  ✓ /var/www/atomclaw/atom-icon.svg"
    echo "  ✓ /etc/nginx/sites-available/atomclaw-toolgateway"
    echo "  ✓ symlinked into sites-enabled/"
    echo "  → run: sudo nginx -t && sudo systemctl reload nginx"
else
    if [[ $(id -u) -ne 0 ]]; then
        echo "  ⏭  not root — skipping nginx install (run with sudo on EC2-B)"
    else
        echo "  ⏭  /etc/nginx not present — skipping (not on the EC2 host)"
    fi
    echo "    Snippet to install manually: $NGINX_SNIPPET"
fi

echo
echo "═══════════════════════════════════════════"
echo "  $BRAND branding applied"
echo "═══════════════════════════════════════════"
echo "Next steps on EC2-B:"
echo "  cd $REPO_DIR"
echo "  make build-ui"
echo "  export REG=localhost:5001/agentregistry-dev/agentregistry"
echo "  export VERSION=\$(git describe --tags --always | head -c 20)"
echo "  sudo docker build -f docker/server.Dockerfile -t \$REG/server:\$VERSION ."
echo "  sudo docker push \$REG/server:\$VERSION"
echo "  sudo docker restart agentregistry-server"
echo "  sudo nginx -t && sudo systemctl reload nginx"
