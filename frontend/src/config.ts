// Public deployment values for the pokedrop-dev stack (see docs/api.md, "Stack outputs").
// These are identifiers, not secrets, so they are committed. If the stack is destroyed and
// recreated they change; re-read them with the command in docs/api.md.
// Deliberately not read from .env files.

const hostedUiDomain = "pokedrop-dev-0affe45b88e7.auth.us-west-2.amazoncognito.com";

export const config = {
  apiUrl: "https://t03xlkdsne.execute-api.us-west-2.amazonaws.com",
  auth: {
    userPoolId: "us-west-2_QmYcPxWp8",
    // The PKCE app client. Not DevClientId, which is for `make token` only.
    clientId: "424drtu0e1eeul6lq8kn6lo82u",
    authority: `https://${hostedUiDomain}`,
    // Must match the registered callback and logout URL exactly (no path, no trailing slash).
    redirectUri: "http://localhost:5173",
    // aws.cognito.signin.user.admin is required by the API's JWT authorizer.
    scope: "openid email aws.cognito.signin.user.admin",
  },
  submission: {
    // Waits between automatic retries of one submission; every attempt reuses the same requestId.
    retryDelaysMs: [500, 1000, 2000],
  },
} as const;
