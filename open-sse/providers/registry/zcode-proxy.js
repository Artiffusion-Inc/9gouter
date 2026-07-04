import { CLAUDE_API_HEADERS } from "../shared.js";

// ZCode start-plan (free quota) OAuth provider.
// Users authorize via browser login at chat.z.ai → zcode.z.ai token exchange → JWT.
// The JWT is sent as Bearer to zcode.z.ai/api/v1/zcode-plan/... endpoints.
// Also supports direct API-key mode (coding-plan) for users who have Z.ai/BigModel keys.
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
      text: "ZCode gives free daily GLM-5.2 quota via browser login (start-plan). OAuth authorizes through Z.ai, then JWT is used for API calls. Coding-plan (API key) also supported — use the glm/glm-cn providers for direct API key access.",
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
    // Custom: zcode uses ?appId= instead of ?client_id= for BigModel,
    // but Z.AI uses standard OAuth2. We use Z.AI flow here.
    callbackPath: "/oauth/callback/zcode",
    extraParams: {
      response_type: "code",
    },
    refreshLeadMs: 432000000, // 5 days
    refresh: {
      encoding: "json",
    },
  },
  // start-plan transport: OpenAI-compatible at zcode.z.ai (JWT Bearer auth)
  // coding-plan: Anthropic at api.z.ai (x-api-key auth)
  transport: {
    // Default: start-plan (free quota) — OpenAI format, JWT Bearer auth
    baseUrl: "https://zcode.z.ai/api/v1/zcode-plan/chat/completions",
    format: "openai",
    headers: {
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
  // Multi-endpoint: coding-plan (Anthropic) and start-plan (OpenAI)
  transports: [
    {
      // start-plan: OpenAI format with JWT Bearer
      format: "openai",
      baseUrl: "https://zcode.z.ai/api/v1/zcode-plan/chat/completions",
      headers: {
        "HTTP-Referer": "https://zcode.z.ai",
        "X-ZCode-Agent": "glm",
        "X-Title": "Z Code@cli",
      },
      auth: { combined: true, header: "Authorization", scheme: "bearer" },
    },
    {
      // coding-plan: Anthropic format with x-api-key
      format: "claude",
      baseUrl: "https://api.z.ai/api/anthropic/v1/messages",
      urlSuffix: "?beta=true",
      headers: { ...CLAUDE_API_HEADERS },
      auth: { combined: true, header: "x-api-key", scheme: "raw" },
    },
  ],
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