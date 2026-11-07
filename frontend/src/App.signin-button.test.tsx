import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { startSignIn } from "./auth/auth";

// jsdom cannot navigate, so replace the redirect itself and check the button triggers it.
vi.mock("./auth/auth", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./auth/auth")>()),
  startSignIn: vi.fn(() => Promise.resolve()),
}));

it("starts the hosted sign-in redirect from the Sign in button", async () => {
  render(<App />);
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(startSignIn).toHaveBeenCalledTimes(1);
});
