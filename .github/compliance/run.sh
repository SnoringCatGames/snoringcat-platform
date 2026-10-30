#!/usr/bin/env bash
# Runs the client SDK's compliance suite standalone (SP50):
# builds the runtime plugin, starts this repo's dev stack
# (infra/dev), registers a throwaway game, and runs the suite in
# a minimal Godot project holding only the SDK addon, GUT, and
# nakama-godot. Before this, Client SDK CI ran no tests at all;
# the suite only ran inside the consuming game's smoke test.
#
#   GODOT=/path/to/godot bash .github/compliance/run.sh
#
# GODOT defaults to `godot` on PATH. Docker must be running.
# Everything happens on loopback with the dev stack's public keys.
set -uo pipefail

godot="${GODOT:-godot}"
gut_tag="v9.7.1"
nakama_godot_tag="v3.4.0"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose="$repo_root/infra/dev/docker-compose.dev.yml"
project_name="snoringcat-sdk-ci"
nakama_port="${SDK_CI_NAKAMA_PORT:-7350}"
console_port="${SDK_CI_CONSOLE_PORT:-7351}"
work="$(mktemp -d)"
trap 'docker compose -p "$project_name" -f "$compose" down -v \
  --remove-orphans >/dev/null 2>&1; rm -rf "$work"' EXIT

# --- Build the runtime plugin -----------------------------------
nakama_tag=$(grep -oE 'heroiclabs/nakama:[0-9.]+' "$compose" \
  | head -1 | cut -d: -f2)
plugin_dir="$repo_root/runtime/build"
mkdir -p "$plugin_dir"
echo "building runtime plugin (nakama-pluginbuilder:$nakama_tag)"
# MSYS_NO_PATHCONV: Git Bash would rewrite -w /backend.
MSYS_NO_PATHCONV=1 docker run --rm -v "$repo_root/runtime:/backend:ro" \
  -v "$plugin_dir:/out" -w /backend \
  "heroiclabs/nakama-pluginbuilder:$nakama_tag" \
  build -buildmode=plugin -trimpath -o /out/snoringcat.so . \
  || exit 1
[ -f "$plugin_dir/snoringcat.so" ] || { echo "plugin build failed"; exit 1; }

# --- Start the stack --------------------------------------------
export HOPNBOP_DEV_NAKAMA_PORT="$nakama_port"
export HOPNBOP_DEV_CONSOLE_PORT="$console_port"
export HOPNBOP_DEV_POSTGRES_PORT="${SDK_CI_POSTGRES_PORT:-5432}"
export HOPNBOP_DEV_PLUGIN_DIR="$plugin_dir"
docker compose -p "$project_name" -f "$compose" up -d || exit 1
base="http://127.0.0.1:$nakama_port"
ready=0
for _ in $(seq 1 120); do
  if curl -fs "$base/healthcheck" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  docker compose -p "$project_name" -f "$compose" logs --tail=60 nakama
  echo "::error::Nakama was not healthy within 120s"
  exit 1
fi

# --- Register the throwaway game --------------------------------
# With unwrap=true the request body IS the RPC payload.
curl -fs -X POST \
  "$base/v2/rpc/register_game?http_key=defaulthttpkey&admin_key=defaultadminkey&unwrap=true" \
  -H 'Content-Type: application/json' \
  -d @"$repo_root/.github/compliance/game.json" \
  >/dev/null || { echo "::error::register_game failed"; exit 1; }
echo "registered game sdkci at $base"

# --- Build the minimal Godot project -----------------------------
proj="$work/project"
mkdir -p "$proj/addons"
cp -R "$repo_root/addons/snoringcat_platform_client" \
  "$proj/addons/snoringcat_platform_client"
curl -fsSL "https://github.com/bitwes/Gut/archive/refs/tags/$gut_tag.zip" \
  -o "$work/gut.zip"
unzip -q "$work/gut.zip" -d "$work/gut-src"
cp -R "$work/gut-src/Gut-${gut_tag#v}/addons/gut" "$proj/addons/gut"
curl -fsSL "https://github.com/heroiclabs/nakama-godot/archive/refs/tags/$nakama_godot_tag.zip" \
  -o "$work/nakama.zip"
unzip -q "$work/nakama.zip" -d "$work/nakama-src"
cp -R "$work/nakama-src/nakama-godot-${nakama_godot_tag#v}/addons/com.heroiclabs.nakama" \
  "$proj/addons/nakama"
cat > "$proj/project.godot" <<'EOF'
; Minimal project for the SDK compliance suite (SP50). Only the
; autoloads the SDK needs; the suite talks HTTP to a local Nakama.
config_version=5

[application]

config/name="snoringcat sdk ci"
config/features=PackedStringArray("4.7")

[autoload]

Platform="*res://addons/snoringcat_platform_client/core/platform.gd"
Nakama="*res://addons/nakama/Nakama.gd"
EOF

"$godot" --headless --path "$proj" --import >"$work/import.log" 2>&1
if ! grep -q '"GutTest"' "$proj/.godot/global_script_class_cache.cfg" \
  2>/dev/null; then
  cat "$work/import.log"
  echo "::error::the import did not register GutTest"
  exit 1
fi

# --- Run the suite -------------------------------------------------
export PLATFORM_API_URL="$base"
export PLATFORM_GAME_ID="sdkci"
export PLATFORM_COMPLIANCE_MODE="live"
export NAKAMA_SERVER_KEY="defaultkey"
export NAKAMA_HTTP_KEY="defaulthttpkey"
export GAME_ADMIN_KEY="defaultadminkey"
export NAKAMA_CONSOLE_URL="http://127.0.0.1:$console_port"
export NAKAMA_CONSOLE_USERNAME="admin"
export NAKAMA_CONSOLE_PASSWORD="devadmin"
export APPDATA="$work/userdata"
export XDG_DATA_HOME="$work/userdata"
mkdir -p "$work/userdata"

suite="res://addons/snoringcat_platform_client/test/compliance"
declared=$(grep -rh '^func test_' \
  "$proj/addons/snoringcat_platform_client/test/compliance" \
  | wc -l | tr -d ' ')
"$godot" --headless --path "$proj" -s addons/gut/gut_cmdln.gd \
  -gdir="$suite" -ginclude_subdirs -gexit -gdisable_colors \
  2>&1 | tee "$work/gut.log"
rc=${PIPESTATUS[0]}
reported=$(grep -oE 'Tests +[0-9]+' "$work/gut.log" | tail -1 \
  | grep -oE '[0-9]+')
echo "compliance: $declared declared, ${reported:-0} reported"
if [ "$rc" -ne 0 ] || ! grep -q 'All tests passed' "$work/gut.log" \
  || [ "$declared" != "${reported:-x}" ]; then
  echo "::error::the compliance run failed (Godot exit $rc)"
  exit 1
fi
echo "compliance-suite: OK"
