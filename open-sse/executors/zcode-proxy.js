import { DefaultExecutor } from "./default.js";
import os from "node:os";

// ZCode identity headers — mirror ZCode desktop client fingerprinting.
// See https://github.com/TriDefender/zcode-api/blob/master/src/proxy/identity.ts
const ASCII_PRINTABLE = /^[\x20-\x7e]+$/;

function normalizeOsCategory(platform) {
  switch (platform) {
    case "darwin": return "macos";
    case "win32": return "windows";
    default: return "linux";
  }
}

function buildRuntimePlatformHeaders() {
  const platform = typeof process?.platform === "string" ? process.platform : "linux";
  const arch = os.arch();
  const release = os.release();
  const headers = {
    "X-Platform": `${platform}-${arch}`,
    "X-Os-Category": normalizeOsCategory(platform),
  };
  if (ASCII_PRINTABLE.test(release)) headers["X-Os-Version"] = release;
  return headers;
}

// ZCode start-plan URL (OpenAI-compatible, JWT Bearer auth)
const STARTPLAN_CHAT_URL = "https://zcode.z.ai/api/v1/zcode-plan/chat/completions";

export class ZcodeProxyExecutor extends DefaultExecutor {
  constructor() {
    super("zcode");
  }

  buildUrl(model, stream, urlIndex = 0, credentials = null) {
    // Runtime transport: use resolved baseUrl from multi-endpoint
    const rt = credentials?.runtimeTransport;
    if (rt?.baseUrl) {
      return rt.urlSuffix ? `${rt.baseUrl}${rt.urlSuffix}` : rt.baseUrl;
    }
    // Default: start-plan OpenAI endpoint
    return STARTPLAN_CHAT_URL;
  }

  buildHeaders(credentials, stream = true) {
    const headers = super.buildHeaders(credentials, stream);

    // Inject ZCode identity headers
    const appVersion = "3.2.2";
    headers["User-Agent"] = `ZCode/${appVersion}`;
    headers["X-ZCode-App-Version"] = appVersion;
    headers["X-ZCode-Agent"] = "glm";
    headers["X-Title"] = "Z Code@cli";
    headers["HTTP-Referer"] = "https://zcode.z.ai";

    // Runtime platform headers
    Object.assign(headers, buildRuntimePlatformHeaders());

    return headers;
  }
}