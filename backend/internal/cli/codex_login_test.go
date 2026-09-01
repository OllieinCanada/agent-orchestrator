package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestCodexLoginRunsSelectedNativeLoginMethod(t *testing.T) {
	tests := []struct {
		name      string
		selection string
		secret    string
		wantArgs  []string
		wantStdin string
	}{
		{name: "ChatGPT browser", selection: "1\n", wantArgs: []string{"-c", `cli_auth_credentials_store="file"`, "login"}},
		{name: "device code", selection: "2\n", wantArgs: []string{"-c", `cli_auth_credentials_store="file"`, "login", "--device-auth"}},
		{name: "API key", selection: "3\n", secret: "sk-test-secret", wantArgs: []string{"-c", `cli_auth_credentials_store="file"`, "login", "--with-api-key"}, wantStdin: "sk-test-secret\n"},
		{name: "access token", selection: "4\n", secret: "token-test-secret", wantArgs: []string{"-c", `cli_auth_credentials_store="file"`, "login", "--with-access-token"}, wantStdin: "token-test-secret\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var gotName string
			var gotArgs []string
			var gotStdin string
			deps := Deps{
				In:       strings.NewReader(tt.selection),
				Out:      &stdout,
				Err:      &stdout,
				LookPath: func(string) (string, error) { return "/usr/local/bin/codex", nil },
				ReadSecret: func(io.Reader) ([]byte, error) {
					return []byte(tt.secret), nil
				},
				ValidateOpenAIAPIKey: func(context.Context, []byte) error { return nil },
				RunInteractiveCommand: func(_ context.Context, name string, args []string, stdin io.Reader, _, _ io.Writer) error {
					gotName = name
					gotArgs = append([]string(nil), args...)
					if stdin != nil {
						data, _ := io.ReadAll(stdin)
						gotStdin = string(data)
					}
					return nil
				},
			}

			cmd := newCodexLoginCommand(&commandContext{deps: deps.withDefaults()})
			cmd.SetIn(deps.In)
			cmd.SetOut(deps.Out)
			cmd.SetErr(deps.Err)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if gotName != "/usr/local/bin/codex" {
				t.Errorf("executable = %q, want resolved Codex", gotName)
			}
			if !slices.Equal(gotArgs, tt.wantArgs) {
				t.Errorf("args = %q, want %q", gotArgs, tt.wantArgs)
			}
			if gotStdin != tt.wantStdin {
				t.Errorf("stdin = %q, want %q", gotStdin, tt.wantStdin)
			}
			if tt.secret != "" && strings.Contains(stdout.String(), tt.secret) {
				t.Fatalf("secret leaked into output: %q", stdout.String())
			}
			for _, arg := range gotArgs {
				if tt.secret != "" && strings.Contains(arg, tt.secret) {
					t.Fatalf("secret leaked into argv: %q", gotArgs)
				}
			}
		})
	}
}

func TestCodexLoginMenuListsEverySupportedMethod(t *testing.T) {
	var stdout bytes.Buffer
	if err := writeCodexLoginMenu(&stdout, codexLoginStyle{}); err != nil {
		t.Fatalf("writeCodexLoginMenu: %v", err)
	}
	for _, fragment := range []string{"Sign in to Codex", "ChatGPT in browser", "Device code", "OpenAI API key", "Access token", "Selection [1-4]"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("menu missing %q:\n%s", fragment, stdout.String())
		}
	}
	const maxRows = 14
	if rows := strings.Count(stdout.String(), "\n") + 1; rows > maxRows {
		t.Fatalf("menu uses %d rows, want at most %d", rows, maxRows)
	}
}

func TestCodexLoginRejectsInvalidAPIKeyBeforeSavingIt(t *testing.T) {
	const secret = "sk-invalid-secret"
	validated := false
	run := false
	var stdout bytes.Buffer
	deps := Deps{
		In:       strings.NewReader("3\n"),
		Out:      &stdout,
		Err:      &stdout,
		LookPath: func(string) (string, error) { return "/codex", nil },
		ReadSecret: func(io.Reader) ([]byte, error) {
			return []byte(secret), nil
		},
		ValidateOpenAIAPIKey: func(_ context.Context, key []byte) error {
			validated = true
			if string(key) != secret {
				t.Fatalf("validated key = %q, want test secret", key)
			}
			return errors.New("OpenAI rejected this API key")
		},
		RunInteractiveCommand: func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
			run = true
			return nil
		},
	}
	cmd := newCodexLoginCommand(&commandContext{deps: deps.withDefaults()})
	cmd.SetIn(deps.In)
	cmd.SetOut(deps.Out)
	cmd.SetErr(deps.Err)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "OpenAI rejected this API key") {
		t.Fatalf("Execute error = %v, want rejected-key error", err)
	}
	if !validated || run {
		t.Fatalf("validated = %v, native login ran = %v", validated, run)
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(err.Error(), secret) {
		t.Fatal("invalid API key leaked into output or error")
	}
}

func TestValidateOpenAIAPIKeyUsesBearerAuthAndRejectsUnauthorized(t *testing.T) {
	const secret = "sk-validation-secret"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q, want bearer test key", got)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader("unauthorized")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	err := validateOpenAIAPIKeyAt(context.Background(), client, "https://api.openai.test/v1/models", []byte(secret))
	if err == nil || !strings.Contains(err.Error(), "OpenAI rejected this API key") {
		t.Fatalf("validation error = %v, want rejected-key error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("validation error leaked the API key")
	}
}

func TestCodexLoginReportsMissingBinaryAndNativeFailure(t *testing.T) {
	tests := []struct {
		name string
		deps Deps
		want string
	}{
		{
			name: "missing binary",
			deps: Deps{LookPath: func(string) (string, error) { return "", errors.New("missing") }},
			want: "codex CLI is not installed",
		},
		{
			name: "native failure",
			deps: Deps{
				In:       strings.NewReader("1\n"),
				LookPath: func(string) (string, error) { return "/codex", nil },
				RunInteractiveCommand: func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
					return errors.New("exit status 1")
				},
			},
			want: "codex login failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newCodexLoginCommand(&commandContext{deps: tt.deps.withDefaults()})
			if tt.deps.In == nil {
				cmd.SetIn(strings.NewReader("1\n"))
			} else {
				cmd.SetIn(tt.deps.In)
			}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Execute error = %v, want %q", err, tt.want)
			}
		})
	}
}
