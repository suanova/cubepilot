# Renders the allowlist openclaw.json injected into every agent Pod (shared
# openclaw-config Secret). Every value comes from setup-time caller input or a
# fixed default -- no host config file is ever read.
#
# $providers is the OpenClaw models.providers object, passed through verbatim,
# so it must match OpenClaw's provider schema: each provider uses "api" as the
# API-style enum (e.g. "openai-completions" for DeepSeek) and "apiKey" for the
# secret, plus baseUrl and models[] (see scripts/setup.sh --help for an example).
#
# Invoke (see scripts/setup.sh):
#   jq -n --argjson providers '<models.providers object>' \
#         --arg defaultModel '<primary model, may be empty>' \
#         --arg token '<gateway auth token>' \
#         -f deploy/openclaw-config.jq

def primary_model:
  if ($defaultModel != "") then $defaultModel
  else ($providers | to_entries[0] | "\(.key)/\(.value.models[0].id)")
  end;

{
  models: { providers: $providers },
  agents: {
    defaults: {
      workspace: "/home/node/.openclaw/workspace",
      model: { primary: primary_model },
      models: ([
        $providers | to_entries[] as $p |
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
