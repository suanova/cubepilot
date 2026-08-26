# Renders the allowlist openclaw.json injected into every agent Pod (shared
# openclaw-config Secret). Every value comes from setup-time caller input or a
# fixed default -- no host config file is ever read.
#
# $providers is the OpenClaw models.providers object. Each provider uses "api"
# as the API-style enum (e.g. "openai-completions" for DeepSeek) and "apiKey"
# for the secret, plus baseUrl and models[] (see scripts/setup.sh --help for an
# example). OpenClaw requires every model entry to carry a non-empty "name"
# (the display label) in addition to the provider-facing "id"; callers commonly
# pass only "id", so the renderer defaults name to id rather than passing a
# config through that fails gateway startup.
#
# Invoke (see scripts/setup.sh):
#   jq -n --argjson providers '<models.providers object>' \
#         --arg defaultModel '<primary model, may be empty>' \
#         --arg token '<gateway auth token>' \
#         -f deploy/openclaw-config.jq

# OpenClaw v2026.6.x validates models[].name as a required non-empty string.
# Default missing names to the id so older id-only provider configs still work.
def normalized_providers:
  $providers | with_entries(
    .value |= (.models //= [] | .models |= map(. + { name: (.name // .id) }))
  );

def primary_model:
  if ($defaultModel != "") then $defaultModel
  else (normalized_providers | to_entries[0] | "\(.key)/\(.value.models[0].id)")
  end;

{
  models: { providers: normalized_providers },
  agents: {
    defaults: {
      workspace: "/home/node/.openclaw/workspace",
      model: { primary: primary_model },
      models: ([
        normalized_providers | to_entries[] as $p |
          (($p.value.models // []) | map({ key: "\($p.key)/\(.id)", value: { alias: (.name // .id) } }))
      ] | add | from_entries // {}),
      sandbox: { mode: "off" }
    }
  },
  gateway: {
    mode: "local",
    port: 18789,
    bind: "lan",
    auth: { mode: "token", token: $token },
    http: { endpoints: { chatCompletions: { enabled: true } } }
  },
  tools: {
    exec: { security: "full", ask: "off" },
    sessions: { visibility: "all" }
  }
}
