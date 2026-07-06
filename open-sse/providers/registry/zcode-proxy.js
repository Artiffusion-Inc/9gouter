// ZCode start-plan (free quota) OAuth provider.
// Users authorize via browser login at chat.z.ai → zcode.z.ai token exchange → JWT.
// The JWT is sent as Bearer to the start-plan upstream, which is Anthropic-native
// (zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages). 9router translates
// OpenAI-format clients to claude before forwarding. Confirmed against the
// ookami42/glm5.2proxy reference implementation.
// See https://github.com/decolua/9router/issues/1869
export default {
  id: "zcode",
  priority: 135,
  alias: "zcode",
  aliases: ["zcode-proxy", "zai"],
  uiAlias: "zc",
  display: {
    name: "ZCode (GLM)",
    icon: "terminal",
    color: "#7C3AED",
    textIcon: "ZC",
    website: "https://zcode.z.ai",
    notice: {
      text: "ZCode gives free daily GLM-5.2 quota via browser login (start-plan). OAuth authorizes through Z.ai, then JWT is used for API calls. The start-plan endpoint is Anthropic-native; OpenAI-format clients are translated automatically. For direct Z.ai API key access, use the glm/glm-cn providers.",
      apiKeyUrl: "https://zcode.z.ai",
    },
  },
  category: "oauth",
  authModes: ["oauth", "apikey"],
  hasOAuth: true,
  // OAuth flow: Z.AI browser auth → zcode.z.ai token exchange → JWT
  oauth: {
    clientId: "client_P8X5CMWmlaRO9gyO-KSqtg",
    authorizeUrl: "https://chat.z.ai/api/oauth/authorize",
    // Token exchange goes through zcode.z.ai (not Z.AI directly)
    tokenUrl: "https://zcode.z.ai/api/v1/oauth/token",
    scope: "openid profile email offline_access",
    codeChallengeMethod: "S256",
    callbackPath: "/oauth/callback/zcode",
    extraParams: {
      response_type: "code",
    },
    refreshLeadMs: 432000000, // 5 days
    refresh: {
      encoding: "json",
    },
  },
  // Start-plan transport: Anthropic-native endpoint, Bearer JWT auth.
  // 9router auto-translates OpenAI client requests to claude format.
  transport: {
    baseUrl: "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages",
    format: "claude",
    headers: {
      "Anthropic-Version": "2023-06-01",
      "HTTP-Referer": "https://zcode.z.ai",
      "X-ZCode-Agent": "glm",
      "X-Title": "Z Code@cli",
    },
    auth: {
      combined: true,
      header: "Authorization",
      scheme: "bearer",
    },
  },
  models: [
    { id: "glm-5.2", name: "GLM 5.2" },
    { id: "glm-5.2-high", name: "GLM 5.2 High" },
    { id: "glm-5.2-max", name: "GLM 5.2 Max" },
    { id: "glm-5-turbo", name: "GLM 5 Turbo" },
    { id: "glm-5v-turbo", name: "GLM 5V Turbo" },
    { id: "glm-5.1", name: "GLM 5.1" },
    { id: "glm-5", name: "GLM 5" },
    { id: "glm-4.7", name: "GLM 4.7" },
    { id: "glm-4.6", name: "GLM 4.6" },
    { id: "glm-4.6v", name: "GLM 4.6V (Vision)" },
    { id: "glm-4.5-air", name: "GLM 4.5 Air" },
  ],
  thinkingConfig: {
    options: ["auto", "on", "off"],
    defaultMode: "auto",
  },
  features: {
    usage: true,
  },
};