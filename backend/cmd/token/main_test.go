package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

func TestParseDotEnv(t *testing.T) {
	in := `# comment
AWS_PROFILE=pokedrop

export TEST_USER_EMAIL = trainer@example.com
TEST_USER_PASSWORD="p@ss word=1"
SINGLE='quoted'
EMPTY=
no-equals-line
=novalue
`
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AWS_PROFILE":        "pokedrop",
		"TEST_USER_EMAIL":    "trainer@example.com",
		"TEST_USER_PASSWORD": "p@ss word=1",
		"SINGLE":             "quoted",
		"EMPTY":              "",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCredentials(t *testing.T) {
	file := writeEnv(t, "TEST_USER_EMAIL=file@example.com\nTEST_USER_PASSWORD=filePass1!\n")
	missing := filepath.Join(t.TempDir(), "absent")

	t.Run("from file", func(t *testing.T) {
		e, p, err := credentials(envFrom(nil), file)
		if err != nil || e != "file@example.com" || p != "filePass1!" {
			t.Fatalf("got %q %q %v", e, p, err)
		}
	})
	t.Run("environment overrides the file per variable", func(t *testing.T) {
		e, p, err := credentials(envFrom(map[string]string{envEmail: "other@example.com"}), file)
		if err != nil || e != "other@example.com" || p != "filePass1!" {
			t.Fatalf("got %q %q %v", e, p, err)
		}
	})
	t.Run("environment alone needs no file", func(t *testing.T) {
		e, p, err := credentials(envFrom(map[string]string{envEmail: "a@b.c", envPassword: "pw"}), missing)
		if err != nil || e != "a@b.c" || p != "pw" {
			t.Fatalf("got %q %q %v", e, p, err)
		}
	})
	t.Run("missing values name the variables", func(t *testing.T) {
		_, _, err := credentials(envFrom(nil), missing)
		if err == nil || !strings.Contains(err.Error(), envEmail) || !strings.Contains(err.Error(), envPassword) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("password is not leaked in errors", func(t *testing.T) {
		_, _, err := credentials(envFrom(map[string]string{envPassword: "s3cret-value"}), missing)
		if err == nil || strings.Contains(err.Error(), "s3cret-value") {
			t.Fatalf("got %v", err)
		}
	})
}

type fakeAPI struct {
	exists       bool
	getErr       error
	createErr    error
	setPassErr   error
	authErr      error
	authOut      *cognitoidentityprovider.AdminInitiateAuthOutput
	calls        []string
	created      *cognitoidentityprovider.AdminCreateUserInput
	passwordSet  *cognitoidentityprovider.AdminSetUserPasswordInput
	authSentFlow types.AuthFlowType
}

func (f *fakeAPI) AdminGetUser(context.Context, *cognitoidentityprovider.AdminGetUserInput, ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminGetUserOutput, error) {
	f.calls = append(f.calls, "get")
	switch {
	case f.getErr != nil:
		return nil, f.getErr
	case f.exists:
		return &cognitoidentityprovider.AdminGetUserOutput{}, nil
	}
	return nil, &types.UserNotFoundException{}
}

func (f *fakeAPI) AdminCreateUser(_ context.Context, in *cognitoidentityprovider.AdminCreateUserInput, _ ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminCreateUserOutput, error) {
	f.calls = append(f.calls, "create")
	f.created = in
	return &cognitoidentityprovider.AdminCreateUserOutput{}, f.createErr
}

func (f *fakeAPI) AdminSetUserPassword(_ context.Context, in *cognitoidentityprovider.AdminSetUserPasswordInput, _ ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminSetUserPasswordOutput, error) {
	f.calls = append(f.calls, "setpw")
	f.passwordSet = in
	return &cognitoidentityprovider.AdminSetUserPasswordOutput{}, f.setPassErr
}

func (f *fakeAPI) AdminInitiateAuth(_ context.Context, in *cognitoidentityprovider.AdminInitiateAuthInput, _ ...func(*cognitoidentityprovider.Options)) (*cognitoidentityprovider.AdminInitiateAuthOutput, error) {
	f.calls = append(f.calls, "auth")
	f.authSentFlow = in.AuthFlow
	if f.authErr != nil {
		return nil, f.authErr
	}
	return f.authOut, nil
}

func tokenOut(tok string) *cognitoidentityprovider.AdminInitiateAuthOutput {
	return &cognitoidentityprovider.AdminInitiateAuthOutput{
		AuthenticationResult: &types.AuthenticationResultType{AccessToken: aws.String(tok)},
	}
}

func TestEnsureUserCreatesMissingUser(t *testing.T) {
	f := &fakeAPI{}
	created, err := ensureUser(context.Background(), f, "pool", "t@example.com", "Passw0rd!")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got := strings.Join(f.calls, ","); got != "get,create,setpw" {
		t.Fatalf("calls = %s", got)
	}
	if f.created.MessageAction != types.MessageActionTypeSuppress {
		t.Error("welcome email must be suppressed")
	}
	if !f.passwordSet.Permanent {
		t.Error("password must be permanent so sign-in needs no challenge")
	}
}

func TestEnsureUserLeavesExistingUserAlone(t *testing.T) {
	f := &fakeAPI{exists: true}
	created, err := ensureUser(context.Background(), f, "pool", "t@example.com", "Passw0rd!")
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got := strings.Join(f.calls, ","); got != "get" {
		t.Fatalf("calls = %s, want only a lookup", got)
	}
}

func TestEnsureUserErrors(t *testing.T) {
	t.Run("lookup failure is not treated as missing", func(t *testing.T) {
		f := &fakeAPI{getErr: errors.New("AccessDenied")}
		if _, err := ensureUser(context.Background(), f, "p", "e", "pw"); err == nil {
			t.Fatal("expected error")
		}
		if len(f.calls) != 1 {
			t.Fatalf("must not create after a failed lookup: %v", f.calls)
		}
	})
	t.Run("password policy failure reports the created user, not the password", func(t *testing.T) {
		f := &fakeAPI{setPassErr: &types.InvalidPasswordException{}}
		created, err := ensureUser(context.Background(), f, "p", "t@example.com", "s3cret-value")
		if err == nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		if strings.Contains(err.Error(), "s3cret-value") || !strings.Contains(err.Error(), "policy") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSignIn(t *testing.T) {
	t.Run("returns the access token using the admin password flow", func(t *testing.T) {
		f := &fakeAPI{authOut: tokenOut("tok")}
		got, err := signIn(context.Background(), f, "p", "c", "e", "pw", kindAccess)
		if err != nil || got != "tok" {
			t.Fatalf("got %q %v", got, err)
		}
		if f.authSentFlow != types.AuthFlowTypeAdminUserPasswordAuth {
			t.Errorf("flow = %s", f.authSentFlow)
		}
	})
	t.Run("wrong password hints at .env.local without leaking it", func(t *testing.T) {
		f := &fakeAPI{authErr: &types.NotAuthorizedException{}}
		_, err := signIn(context.Background(), f, "p", "c", "e", "s3cret-value", kindAccess)
		if err == nil || !strings.Contains(err.Error(), envPassword) || strings.Contains(err.Error(), "s3cret-value") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a challenge is an error", func(t *testing.T) {
		f := &fakeAPI{authOut: &cognitoidentityprovider.AdminInitiateAuthOutput{ChallengeName: types.ChallengeNameTypeNewPasswordRequired}}
		if _, err := signIn(context.Background(), f, "p", "c", "e", "pw", kindAccess); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("no token is an error", func(t *testing.T) {
		f := &fakeAPI{authOut: &cognitoidentityprovider.AdminInitiateAuthOutput{}}
		if _, err := signIn(context.Background(), f, "p", "c", "e", "pw", kindAccess); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestSignInTokenKinds(t *testing.T) {
	out := &cognitoidentityprovider.AdminInitiateAuthOutput{
		AuthenticationResult: &types.AuthenticationResultType{AccessToken: aws.String("acc"), IdToken: aws.String("idt")},
	}
	for kind, want := range map[string]string{kindAccess: "acc", kindID: "idt"} {
		got, err := signIn(context.Background(), &fakeAPI{authOut: out}, "p", "c", "e", "pw", kind)
		if err != nil || got != want {
			t.Errorf("%s: got %q %v, want %q", kind, got, err, want)
		}
	}
	if _, err := signIn(context.Background(), &fakeAPI{authOut: tokenOut("acc")}, "p", "c", "e", "pw", kindID); err == nil {
		t.Error("a missing ID token must be an error, not an empty string")
	}
}

func TestRequireOutputs(t *testing.T) {
	p, c, err := requireOutputs(map[string]string{outputUserPoolID: "pool", outputDevClientID: "client"}, "s")
	if err != nil || p != "pool" || c != "client" {
		t.Fatalf("got %q %q %v", p, c, err)
	}
	if _, _, err := requireOutputs(map[string]string{outputUserPoolID: "pool"}, "s"); err == nil || !strings.Contains(err.Error(), outputDevClientID) {
		t.Fatalf("missing dev client: %v", err)
	}
	if _, _, err := requireOutputs(nil, "s"); err == nil || !strings.Contains(err.Error(), outputUserPoolID) {
		t.Fatalf("missing pool: %v", err)
	}
}

func TestRunRefusesProd(t *testing.T) {
	if _, err := run(context.Background(), "prod", "unused", kindAccess); err == nil || !strings.Contains(err.Error(), "prod") {
		t.Fatalf("got %v", err)
	}
}
