#!/usr/bin/env bash
# Unit test for deploy/openclaw-config.jq (the allowlist openclaw.json generator).
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
JQ_FILE="$REPO_DIR/deploy/openclaw-config.jq"
[ -f "$JQ_FILE" ] || { echo "missing $JQ_FILE"; exit 1; }

# OpenClaw provider shape: api is the API-style enum, apiKey is the secret.
PROVIDERS='{"deepseek":{"api":"openai-completions","apiKey":"sk-test","baseUrl":"https://api.deepseek.com","models":[{"id":"deepseek-v4-flash","name":"DeepSeek V4 Flash"},{"id":"deepseek-chat","name":"DeepSeek Chat"}]}}'

out() { jq -n --argjson providers "$PROVIDERS" --arg defaultModel "$1" --arg token "$2" -f "$JQ_FILE"; }

# 1. default primary = first provider / first model
v="$(out "" "tok" | jq -r '.agents.defaults.model.primary')"
[ "$v" = "deepseek/deepseek-v4-flash" ] || { echo "FAIL: primary=$v"; exit 1; }

# 2. explicit default model wins
v="$(out "deepseek/deepseek-chat" "tok" | jq -r '.agents.defaults.model.primary')"
[ "$v" = "deepseek/deepseek-chat" ] || { echo "FAIL: explicit primary=$v"; exit 1; }

# 3. model catalog derived with aliases
v="$(out "" "tok" | jq -r '.agents.defaults.models["deepseek/deepseek-v4-flash"].alias')"
[ "$v" = "DeepSeek V4 Flash" ] || { echo "FAIL: alias=$v"; exit 1; }
v="$(out "" "tok" | jq -r '.agents.defaults.models | length')"
[ "$v" = "2" ] || { echo "FAIL: catalog length=$v"; exit 1; }

# 4. token injected into gateway.auth.token
v="$(out "" "tok123" | jq -r '.gateway.auth.token')"
[ "$v" = "tok123" ] || { echo "FAIL: token=$v"; exit 1; }

# 5. provider credentials pass through (apiKey = secret, api = style enum)
v="$(out "" "tok" | jq -r '.models.providers.deepseek.apiKey')"
[ "$v" = "sk-test" ] || { echo "FAIL: apiKey=$v"; exit 1; }
v="$(out "" "tok" | jq -r '.models.providers.deepseek.api')"
[ "$v" = "openai-completions" ] || { echo "FAIL: api=$v"; exit 1; }

# 6. fixed defaults present
for expr in \
  '.gateway.mode == "local"' \
  '.gateway.port == 18789' \
  '.gateway.bind == "lan"' \
  '.gateway.auth.mode == "token"' \
  '.gateway.http.endpoints.chatCompletions.enabled == true' \
  '.agents.defaults.workspace == "/home/node/.openclaw/workspace"' \
  '.agents.defaults.sandbox.mode == "off"' \
  '.tools.exec.security == "full"' \
  '.tools.exec.ask == "off"' \
  '.tools.sessions.visibility == "all"' \
  '.models.providers.deepseek.baseUrl == "https://api.deepseek.com"'; do
  out "" "tok" | jq -e "$expr" >/dev/null || { echo "FAIL: $expr"; exit 1; }
done

echo "openclaw-config.jq: all tests passed"
