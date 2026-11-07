export function fakeJwt(payload: object): string {
  const encode = (value: object) => btoa(JSON.stringify(value)).replace(/=+$/, "");
  return `${encode({ alg: "none" })}.${encode(payload)}.sig`;
}

export const ACCESS_TOKEN = fakeJwt({ sub: "trainer-1", token_use: "access" });

/** Put a valid session in sessionStorage, as a completed sign-in would. */
export function signInForTest(accessToken = ACCESS_TOKEN): void {
  sessionStorage.setItem(
    "pokedrop.session",
    JSON.stringify({ accessToken, expiresAt: Date.now() + 60 * 60 * 1000 }),
  );
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

export const CARDS = [
  { cardId: "card-001", name: "Ember Drake", rarity: "rare", available: 12 },
  { cardId: "card-002", name: "Tide Sprite", rarity: "common", available: 0 },
  { cardId: "card-003", name: "Gale Warden", rarity: "epic", available: 3 },
];
