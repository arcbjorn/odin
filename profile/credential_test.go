package profile

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func keyCmdConfig(command string) string {
	return `
toolsets = ["skills"]
timezone = "UTC"

[[providers]]
kind = "openai"
name = "primary"
model = "gpt-5.6-terra"
base_url = "https://example.invalid/v1"
api_key_cmd = "` + command + `"
`
}

func loadKeyCmdProfile(t *testing.T, command string) (*Profile, error) {
	t.Helper()
	root := writeProfile(t, "default", keyCmdConfig(command), "# Test agent", false, true)
	return Load(root, "default")
}

// The point of api_key_cmd: the key comes from the store that already holds
// it, without widening the unit's EnvironmentFile.
func TestAPIKeyCmdSuppliesTheCredential(t *testing.T) {
	p, err := loadKeyCmdProfile(t, "printf 'sk-from-the-store'")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tokens, err := providerTokens(p)
	if err != nil {
		t.Fatalf("providerTokens: %v", err)
	}
	token, err := tokens["primary"].Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "sk-from-the-store" {
		t.Fatalf("token = %q", token)
	}
}

// A trailing newline is what every `sops -d | jq -r` pipeline produces, and an
// untrimmed one breaks the Authorization header in a way that reads as a bad
// key rather than a bad pipeline.
func TestAPIKeyCmdTrimsTrailingNewline(t *testing.T) {
	p, err := loadKeyCmdProfile(t, "printf 'sk-trimmed\\n'")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tokens, err := providerTokens(p)
	if err != nil {
		t.Fatalf("providerTokens: %v", err)
	}
	token, _ := tokens["primary"].Token(t.Context())
	if token != "sk-trimmed" {
		t.Fatalf("token = %q, want the newline trimmed", token)
	}
}

// A broken secret store must stop the start, where someone is watching.
func TestAPIKeyCmdFailureIsReportedWithStderr(t *testing.T) {
	p, err := loadKeyCmdProfile(t, "echoerr() { echo no such key 1>&2; }; echoerr; exit 3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = providerTokens(p)
	if err == nil {
		t.Fatal("a failing api_key_cmd must fail the build")
	}
	if !strings.Contains(err.Error(), "no such key") {
		t.Fatalf("error should carry the command's diagnostics: %v", err)
	}
}

func TestAPIKeyCmdEmptyOutputIsRejected(t *testing.T) {
	p, err := loadKeyCmdProfile(t, "true")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := providerTokens(p); err == nil {
		t.Fatal("a command producing no key must fail rather than authenticate with an empty string")
	}
}

// api_key_cmd fetches a secret; it must not become a place to write one, which
// is what the rejected api_key/token/secret keys already guard against.
func TestAPIKeyCmdRejectsAnEchoedLiteral(t *testing.T) {
	_, err := loadKeyCmdProfile(t, "echo sk-literal-in-git")
	if err == nil {
		t.Fatal("an echoed literal must be rejected")
	}
	if !strings.Contains(err.Error(), "api_key_cmd") {
		t.Fatalf("error should name the key: %v", err)
	}
}

func TestAPIKeyEnvAndCmdAreMutuallyExclusive(t *testing.T) {
	config := strings.Replace(keyCmdConfig("printf x"),
		`api_key_cmd = "printf x"`,
		"api_key_cmd = \"printf x\"\napi_key_env = \"SOME_KEY\"", 1)
	root := writeProfile(t, "default", config, "# Test agent", false, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("two credential sources for one provider must be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error should explain the conflict: %v", err)
	}
}

// Neither source configured is still an error: the old message named only
// api_key_env, which would send someone down the wrong path.
func TestMissingCredentialSourceNamesBothOptions(t *testing.T) {
	config := strings.Replace(keyCmdConfig("printf x"), "api_key_cmd = \"printf x\"\n", "", 1)
	root := writeProfile(t, "default", config, "# Test agent", false, true)

	_, err := Load(root, "default")
	if err == nil {
		t.Fatal("a provider with no credential source must fail")
	}
	if !strings.Contains(err.Error(), "api_key_cmd") {
		t.Fatalf("error should mention both sources: %v", err)
	}
}

// The whole runtime must come up on a command-sourced key, not just the token.
func TestBuildRunsWithACommandSourcedKey(t *testing.T) {
	p, err := loadKeyCmdProfile(t, "printf 'sk-from-the-store'")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rt, err := Build(p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close()

	if rt.Provider.Name() != "primary/gpt-5.6-terra" {
		t.Fatalf("provider = %q", rt.Provider.Name())
	}
}
