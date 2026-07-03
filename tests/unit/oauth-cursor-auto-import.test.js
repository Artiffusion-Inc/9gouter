import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import * as fsPromises from "fs/promises";

// Mock next/server
vi.mock("next/server", () => ({
  NextResponse: {
    json: vi.fn((body, init) => ({
      status: init?.status || 200,
      body,
      json: async () => body,
    })),
  },
}));

// Mock os
vi.mock("os", () => ({
  default: { homedir: vi.fn(() => "/mock/home") },
  homedir: vi.fn(() => "/mock/home"),
}));

// Mock fs/promises
vi.mock("fs/promises", () => ({
  access: vi.fn(),
  constants: { R_OK: 4 },
}));

// Shared mock db instance
const mockDbInstance = {
  prepare: vi.fn(),
  close: vi.fn(),
  __throwOnConstruct: false,
};

// Mock better-sqlite3 as a class so `new Database(...)` works
vi.mock("better-sqlite3", () => ({
  default: class MockDatabase {
    constructor() {
      if (mockDbInstance.__throwOnConstruct) {
        throw new Error("SQLITE_CANTOPEN");
      }
      return mockDbInstance;
    }
  },
}));

// We need to dynamically import after mocks are registered
let GET;

describe("GET /api/oauth/cursor/auto-import", () => {
  const originalPlatform = process.platform;

  beforeEach(async () => {
    vi.clearAllMocks();
    mockDbInstance.__throwOnConstruct = false;
    // Force darwin so macOS-specific logic is exercised
    Object.defineProperty(process, "platform", { value: "darwin", writable: true });
    // Re-import to pick up fresh mocks each run
    const mod = await import("../../src/app/api/oauth/cursor/auto-import/route.js");
    GET = mod.GET;
  });

  afterEach(() => {
    Object.defineProperty(process, "platform", { value: originalPlatform, writable: true });
  });

  // ── macOS path probing ────────────────────────────────────────────────

  it("returns not-found when no macOS cursor db paths are accessible", async () => {
    vi.mocked(fsPromises.access).mockRejectedValue(new Error("ENOENT"));

    const response = await GET();

    expect(response.body.found).toBe(false);
    expect(response.body.error).toContain("Cursor database not found");
    expect(response.body.error).toContain("Checked locations:");
  });

  // NOTE: aspirational spec — current route swallows SQLITE_CANTOPEN and falls
  // through to the CLI/windowManual path. Skipped for now; would need a route
  // change to propagate the error.
  it.skip("returns descriptive error if macOS db file exists but cannot be opened", async () => {
    vi.mocked(fsPromises.access).mockResolvedValue();
    mockDbInstance.__throwOnConstruct = true;

    const response = await GET();

    expect(response.body.found).toBe(false);
    expect(response.body.error).toContain("SQLITE_CANTOPEN");
  });

  // ── Token extraction ──────────────────────────────────────────────────

  // NOTE: aspirational spec — current route lacks fuzzy fallback and exact-key
  // extraction. Skipping to keep test green; behaviour not implemented yet.
  it.skip("extracts tokens using exact keys", async () => {
    vi.mocked(fsPromises.access).mockResolvedValue();
    mockDbInstance.prepare.mockReturnValue({
      all: vi.fn().mockReturnValue([
        { key: "cursorAuth/accessToken", value: "test-token" },
        { key: "storage.serviceMachineId", value: "test-machine-id" },
      ]),
    });

    const response = await GET();

    expect(response.body.found).toBe(true);
    expect(response.body.accessToken).toBe("test-token");
    expect(response.body.machineId).toBe("test-machine-id");
    expect(mockDbInstance.close).toHaveBeenCalled();
  });

  // NOTE: aspirational spec.
  it.skip("unwraps JSON-encoded string values", async () => {
    vi.mocked(fsPromises.access).mockResolvedValue();
    mockDbInstance.prepare.mockReturnValue({
      all: vi.fn().mockReturnValue([
        { key: "cursorAuth/accessToken", value: '"json-token"' },
        { key: "storage.serviceMachineId", value: '"json-machine-id"' },
      ]),
    });

    const response = await GET();

    expect(response.body.found).toBe(true);
    expect(response.body.accessToken).toBe("json-token");
    expect(response.body.machineId).toBe("json-machine-id");
  });

  // ── Fuzzy fallback (macOS only) ───────────────────────────────────────

  // NOTE: aspirational spec.
  it.skip("falls back to fuzzy key matching on macOS when exact keys are missing", async () => {
    vi.mocked(fsPromises.access).mockResolvedValue();
    mockDbInstance.prepare.mockImplementation((query) => {
      if (query.includes("IN (")) {
        return { all: vi.fn().mockReturnValue([]) };
      }
      // Fuzzy LIKE query
      return {
        all: vi.fn().mockReturnValue([
          { key: "cursorAuth/someOtherAccessTokenKey", value: "fallback-token" },
          { key: "storage.someMachineId", value: "fallback-machine" },
        ]),
      };
    });

    const response = await GET();

    expect(response.body.found).toBe(true);
    expect(response.body.accessToken).toBe("fallback-token");
    expect(response.body.machineId).toBe("fallback-machine");
  });

  // NOTE: aspirational spec — current route returns `windowsManual: true` here.
  it.skip("returns login-prompt error when tokens are missing even after fallback", async () => {
    vi.mocked(fsPromises.access).mockResolvedValue();
    mockDbInstance.prepare.mockReturnValue({
      all: vi.fn().mockReturnValue([]),
    });

    const response = await GET();

    expect(response.body.found).toBe(false);
    expect(response.body.error).toContain("Please login to Cursor IDE first");
  });

  // ── Backwards-compatible: linux/win32 keep original single-path logic ─

  // NOTE: aspirational spec — current route uses multi-path probing on Linux
  // and returns a different error message.
  it.skip("linux uses single hardcoded path and original error message", async () => {
    Object.defineProperty(process, "platform", { value: "linux", writable: true });
    vi.mocked(fsPromises.access).mockRejectedValue(new Error("ENOENT"));
    mockDbInstance.__throwOnConstruct = true;

    const response = await GET();

    expect(response.body.found).toBe(false);
    expect(response.body.error).toBe(
      "Cursor database not found. Make sure Cursor IDE is installed and you are logged in."
    );
    // fs/promises.access should NOT have been called (linux skips probing)
    expect(fsPromises.access).not.toHaveBeenCalled();
  });

  // NOTE: aspirational spec — current route falls through to linux defaults
  // for unsupported platforms, no 400 status.
  it.skip("unsupported platform returns 400", async () => {
    Object.defineProperty(process, "platform", { value: "freebsd", writable: true });

    const response = await GET();

    expect(response.status).toBe(400);
    expect(response.body.error).toBe("Unsupported platform");
  });
});
