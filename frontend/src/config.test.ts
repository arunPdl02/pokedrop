import { afterEach, describe, expect, test, vi } from "vitest";
import { ConfigError, applyConfig, config, loadConfig, parseConfig } from "./config";
import { jsonResponse } from "./test-utils";

const VALID = {
  apiUrl: "https://abc123.execute-api.us-west-2.amazonaws.com",
  clientId: "client-1",
  hostedUiDomain: "pokedrop-prod-abc.auth.us-west-2.amazoncognito.com",
};

// loadConfig and applyConfig write to the shared `config`; put the test values back afterwards.
afterEach(() => {
  applyConfig(
    { apiUrl: "https://api.test.example", clientId: "test-client-id", hostedUiDomain: "auth.test.example" },
    "http://localhost:5173",
  );
});

describe("parseConfig", () => {
  test("accepts a complete config", () => {
    expect(parseConfig(VALID)).toEqual(VALID);
  });

  test("drops a trailing slash from the API URL and ignores extra fields", () => {
    expect(parseConfig({ ...VALID, apiUrl: `${VALID.apiUrl}//`, userPoolId: "x" })).toEqual(VALID);
  });

  test.each(["apiUrl", "clientId", "hostedUiDomain"] as const)("rejects a missing %s", (key) => {
    const raw: Record<string, unknown> = { ...VALID };
    delete raw[key];
    expect(() => parseConfig(raw)).toThrow(ConfigError);
    expect(() => parseConfig(raw)).toThrow(key);
  });

  test.each(["apiUrl", "clientId", "hostedUiDomain"] as const)("rejects an empty or non-string %s", (key) => {
    expect(() => parseConfig({ ...VALID, [key]: "  " })).toThrow(ConfigError);
    expect(() => parseConfig({ ...VALID, [key]: 7 })).toThrow(ConfigError);
  });

  test("rejects values that are not a JSON object", () => {
    for (const raw of [null, "text", 7, [], undefined]) {
      expect(() => parseConfig(raw)).toThrow(ConfigError);
    }
  });

  test("rejects an API URL that is not an http(s) URL", () => {
    expect(() => parseConfig({ ...VALID, apiUrl: "not a url" })).toThrow(ConfigError);
    expect(() => parseConfig({ ...VALID, apiUrl: "javascript:alert(1)" })).toThrow(ConfigError);
  });

  test("rejects a hosted UI domain with a scheme, path or port", () => {
    for (const hostedUiDomain of [`https://${VALID.hostedUiDomain}`, `${VALID.hostedUiDomain}/login`, "host:443"]) {
      expect(() => parseConfig({ ...VALID, hostedUiDomain })).toThrow(ConfigError);
    }
  });
});

describe("applyConfig", () => {
  test("derives the authority and uses the current origin as the redirect URI", () => {
    applyConfig(VALID, "https://dxyz.cloudfront.net");
    expect(config.apiUrl).toBe(VALID.apiUrl);
    expect(config.auth.clientId).toBe("client-1");
    expect(config.auth.authority).toBe(`https://${VALID.hostedUiDomain}`);
    expect(config.auth.redirectUri).toBe("https://dxyz.cloudfront.net");
  });
});

describe("loadConfig", () => {
  test("fetches /config.json uncached and applies it", async () => {
    const fetchFn = vi.fn().mockResolvedValue(jsonResponse(VALID));
    await loadConfig(fetchFn);
    expect(fetchFn).toHaveBeenCalledWith("/config.json", { cache: "no-store" });
    expect(config.apiUrl).toBe(VALID.apiUrl);
    expect(config.auth.redirectUri).toBe(window.location.origin);
  });

  test("fails on a non-200 response and leaves the config alone", async () => {
    const before = config.apiUrl;
    await expect(loadConfig(vi.fn().mockResolvedValue(new Response("nope", { status: 404 })))).rejects.toThrow(/404/);
    expect(config.apiUrl).toBe(before);
  });

  test("fails on a response that is not JSON, such as an HTML fallback page", async () => {
    const html = new Response("<!doctype html>", { status: 200, headers: { "Content-Type": "text/html" } });
    await expect(loadConfig(vi.fn().mockResolvedValue(html))).rejects.toThrow(/not valid JSON/);
  });

  test("fails when a field is missing", async () => {
    const { clientId: _omitted, ...incomplete } = VALID;
    await expect(loadConfig(vi.fn().mockResolvedValue(jsonResponse(incomplete)))).rejects.toThrow(/clientId/);
  });

  test("fails with a ConfigError when the request itself fails", async () => {
    const error = await loadConfig(vi.fn().mockRejectedValue(new TypeError("offline"))).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ConfigError);
  });
});
