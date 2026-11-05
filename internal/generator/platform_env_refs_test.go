package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeneratedPlatformEnvironmentReferences(t *testing.T) {
	t.Parallel()
	outputDir := filepath.Join(t.TempDir(), "env-profile-pp-cli")
	require.NoError(t, New(minimalSpec("env-profile"), outputDir).Generate())
	source := strings.ReplaceAll(platformEnvironmentReferenceTests, "__MODULE__", generatedModulePath(t, outputDir))
	require.NoError(t, os.WriteFile(filepath.Join(outputDir, "internal", "cli", "platform_env_refs_test.go"), []byte(source), 0o644))
	requireGeneratedCompiles(t, outputDir)
	runGoCommandRequired(t, outputDir, "test", "./internal/cli", "-run", "TestEnvironmentReferences", "-count=1")
}

const platformEnvironmentReferenceTests = `package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"__MODULE__/internal/platform"
)

type environmentIdentityAdapter struct {
	calls    int
	observed platform.ObservedIdentity
}

func (a *environmentIdentityAdapter) EndpointClass() string { return "identity" }
func (a *environmentIdentityAdapter) ProbeIdentity(_ context.Context, credentials platform.ResolvedCredentials, _ platform.SourceProfile) (platform.ObservedIdentity, error) {
	a.calls++
	for key, want := range map[string]string{"credential": os.Getenv("PP_TEST_KEY"), "username": "synthetic-user-value", "extra": "synthetic-extra-value"} {
		if string(credentials[key]) != want {
			return platform.ObservedIdentity{}, errors.New("incorrect credential passed to probe")
		}
	}
	return a.observed, nil
}

type environmentProviderResolver struct{ calls int }

func (r *environmentProviderResolver) Resolve(_ context.Context, ref string) ([]byte, error) {
	r.calls++
	if ref != "op://fixture/item/user" {
		return nil, errors.New("environment reference reached provider resolver")
	}
	return []byte("synthetic-user-value"), nil
}

func TestEnvironmentReferences(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, filepath.Join(home, name))
	}
	t.Setenv("PRINTING_PRESS_CLIENT_PROFILE", "")
	for _, name := range []string{"PRINTING_PRESS_VERIFY", "PRINTING_PRESS_DOGFOOD", "PRINTING_PRESS_VERIFY_LIVE_HTTP", "PRINTING_PRESS_MCP_BOUND_PROFILE"} {
		t.Setenv(name, "")
	}
	for name, value := range map[string]string{"PP_TEST_KEY": "synthetic-key-value", "PP_TEST_USER": "synthetic-user-value", "PP_TEST_EXTRA": "synthetic-extra-value"} {
		t.Setenv(name, value)
	}
	adapter := &environmentIdentityAdapter{observed: platform.ObservedIdentity{AccountID: "acct", OrgName: "org", StoreID: "store", StoreDomain: "store.example", PropertyID: "123", BaseURL: "https://tenant.example"}}
	previous := registeredPlatformSource
	t.Cleanup(func() { registeredPlatformSource = previous })
	validations := 0
	registeredPlatformSource = &platformSourceRegistration{
		Source: "selected", Adapter: adapter,
		CredentialResolverFactory: func() platform.CredentialResolver { t.Fatal("env refs invoked provider resolver"); return nil },
		ValidateSourceProfile:     func(source platform.SourceProfile) error { validations++; return source.Validate() },
	}
	run := func(args ...string) error {
		t.Helper()
		var output bytes.Buffer
		root := RootCmd()
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		err := root.Execute()
		for _, secret := range []string{"synthetic-key-value", "synthetic-rotated-key", "synthetic-user-value", "synthetic-extra-value"} {
			if strings.Contains(output.String(), secret) || (err != nil && strings.Contains(err.Error(), secret)) {
				t.Fatal("credential leaked into output")
			}
		}
		return err
	}
	if err := run("client", "add", "pp-test-env-tenant", "--credential-ref", "env:PP_TEST_KEY", "--username-ref", "env:PP_TEST_USER", "--additional-credential-ref", "extra=env:PP_TEST_EXTRA", "--expected-account-id", "acct", "--expected-org-name", "org", "--expected-store-id", "store", "--expected-store-domain", "store.example", "--expected-property-id", "123", "--expected-base-url", "https://tenant.example", "--json"); err != nil {
		t.Fatal(err)
	}
	registeredPlatformSource.CredentialResolverFactory = nil
	if err := run("client", "show", "pp-test-env-tenant", "--json"); err != nil {
		t.Fatal(err)
	}
	if err := run("whoami", "--client-profile", "pp-test-env-tenant", "--receipt", "--json"); err != nil {
		t.Fatal(err)
	}
	if adapter.calls == 0 || validations == 0 {
		t.Fatal("env refs bypassed validation or identity probe")
	}
	t.Setenv("PP_TEST_KEY", "synthetic-rotated-key")
	callsBeforeRotation := adapter.calls
	if err := run("whoami", "--client-profile", "pp-test-env-tenant", "--json"); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != callsBeforeRotation+1 {
		t.Fatal("rotated credential did not trigger a fresh identity probe")
	}
	t.Setenv("PP_TEST_KEY", "synthetic-key-value")
	t.Setenv("PRINTING_PRESS_CLIENT_PROFILE", "pp-test-env-tenant")
	if err := BindMCPServerProfile(); err != nil {
		t.Fatal(err)
	}
	session, err := VerifyMCPInvocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	session.ZeroCredentials()
	t.Setenv("PRINTING_PRESS_MCP_BOUND_PROFILE", "")
	// Every identity assertion must still reject a mismatch with env credentials.
	for _, field := range []string{"account", "org", "store", "domain", "property", "base"} {
		saved := adapter.observed
		switch field {
		case "account":
			adapter.observed.AccountID = "wrong"
		case "org":
			adapter.observed.OrgName = "wrong"
		case "store":
			adapter.observed.StoreID = "wrong"
		case "domain":
			adapter.observed.StoreDomain = "wrong.example"
		case "property":
			adapter.observed.PropertyID = "999"
		case "base":
			adapter.observed.BaseURL = "https://wrong.example"
		}
		if err := run("whoami", "--client-profile", "pp-test-env-tenant", "--json"); err == nil {
			t.Fatalf("%s mismatch accepted", field)
		}
		adapter.observed = saved
	}
	for _, name := range []string{"PP_TEST_KEY", "PP_TEST_USER", "PP_TEST_EXTRA"} {
		value := os.Getenv(name)
		for _, unset := range []bool{false, true} {
			t.Setenv(name, "")
			if unset {
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			calls := adapter.calls
			err := run("whoami", "--client-profile", "pp-test-env-tenant", "--receipt", "--json")
			if err == nil || !strings.Contains(err.Error(), "credential environment variable "+name+" is unset or empty") || adapter.calls != calls {
				t.Fatalf("missing env failed to stop probe: %v", err)
			}
		}
		t.Setenv(name, value)
	}
	for _, ref := range []string{"env:", "env:1BAD", "env:A-B", "env: KEY", "env:KEY\n", "env:$KEY", "env:A=B"} {
		if err := (platform.SourceProfile{CredentialRef: ref, ExpectedAccountID: "acct"}).Validate(); err == nil {
			t.Fatalf("invalid env reference accepted: %q", ref)
		}
	}
	// Mixed profiles still require the downstream resolver and validation hook.
	mixed := platform.SourceProfile{CredentialRef: "env:PP_TEST_KEY", UsernameRef: "op://fixture/item/user", ExpectedAccountID: "acct"}
	if _, err := platformCredentialResolver(mixed); err == nil || !strings.Contains(err.Error(), "CredentialResolverFactory") {
		t.Fatalf("mixed profile lost resolver requirement: %v", err)
	}
	provider := &environmentProviderResolver{}
	registeredPlatformSource.CredentialResolverFactory = func() platform.CredentialResolver { return provider }
	if err := run("client", "add", "pp-test-mixed", "--credential-ref", "env:PP_TEST_KEY", "--username-ref", "op://fixture/item/user", "--additional-credential-ref", "extra=env:PP_TEST_EXTRA", "--expected-account-id", "acct", "--json"); err != nil {
		t.Fatal(err)
	}
	if err := run("client", "show", "pp-test-mixed", "--json"); err != nil {
		t.Fatal(err)
	}
	if err := run("whoami", "--client-profile", "pp-test-mixed", "--json"); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider resolver calls = %d, want 1", provider.calls)
	}
	validator := registeredPlatformSource.ValidateSourceProfile
	registeredPlatformSource.ValidateSourceProfile = nil
	if err := run("whoami", "--client-profile", "pp-test-env-tenant"); err == nil || !strings.Contains(err.Error(), "ValidateSourceProfile") {
		t.Fatalf("env profile lost validation requirement: %v", err)
	}
	registeredPlatformSource.ValidateSourceProfile = validator
	registeredPlatformSource.ValidateSourceProfile = func(platform.SourceProfile) error { return errors.New("source assertion rejected") }
	if err := run("whoami", "--client-profile", "pp-test-env-tenant"); err == nil || !strings.Contains(err.Error(), "source assertion rejected") {
		t.Fatalf("source validation skipped: %v", err)
	}
	if err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range []string{"synthetic-key-value", "synthetic-rotated-key", "synthetic-user-value", "synthetic-extra-value"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatal("credential persisted to disk")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
`
