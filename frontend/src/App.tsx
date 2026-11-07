import { AuthProvider, useAuth } from "./auth/AuthProvider";

function Dashboard() {
  const { state, signIn, signOut } = useAuth();

  if (state.status === "loading") return <p>Signing you in…</p>;
  if (state.status === "signedOut") {
    return (
      <>
        {state.message && <p role="alert">{state.message}</p>}
        <button type="button" onClick={signIn}>
          Sign in
        </button>
      </>
    );
  }
  return (
    <button type="button" onClick={signOut}>
      Sign out
    </button>
  );
}

export function App() {
  return (
    <AuthProvider>
      <h1>PokéDrop</h1>
      <Dashboard />
    </AuthProvider>
  );
}
