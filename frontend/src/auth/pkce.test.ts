import { base64Url, challengeFor, randomString } from "./pkce";

describe("pkce", () => {
  it("matches the S256 example from RFC 7636 appendix B", async () => {
    expect(await challengeFor("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")).toBe(
      "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
    );
  });

  it("encodes base64url without padding", () => {
    expect(base64Url(new Uint8Array([251, 255, 254]))).toBe("-__-");
    expect(base64Url(new Uint8Array([1]))).toBe("AQ");
  });

  it("generates verifiers within the 43-128 character RFC range", () => {
    const verifier = randomString(32);
    expect(verifier).toMatch(/^[A-Za-z0-9_-]{43,128}$/);
    expect(randomString(32)).not.toBe(verifier);
  });
});
