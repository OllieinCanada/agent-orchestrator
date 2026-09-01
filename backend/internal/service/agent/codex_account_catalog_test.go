package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const testAccountID = "72d4db6e-da2c-414c-a6a9-fdbd09a006b6"

func commitTestAccount(t *testing.T, catalog *codexAccountCatalog, pendingRoot, operationID string, observed ports.CodexAccountObservation) codexAccountRecord {
	t.Helper()
	pendingDir, home, err := createPendingCredentialHome(pendingRoot, operationID)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not JSON: the vault treats Codex credentials as opaque bytes.
	if err := writePrivateFileAtomic(filepath.Join(home, codexCredentialFilename), []byte("opaque-codex-credential\x00\xff")); err != nil {
		t.Fatal(err)
	}
	record, err := catalog.commitPending(pendingDir, observed)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestCodexAccountCatalogCommitsStrictPrivateOpaqueSlot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	pending := filepath.Join(filepath.Dir(root), "pending-accounts")
	catalog := newCodexAccountCatalog(root, nil)
	catalog.newID = func() string { return testAccountID }
	catalog.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	email := "person@example.com"
	record := commitTestAccount(t, catalog, pending, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{
		Authentication: domain.AgentAuthenticationAuthorized,
		Method:         domain.CodexAuthMethodChatGPT,
		Email:          &email,
	})
	if record.Snapshot.ID != testAccountID || record.Snapshot.Label != email || record.Snapshot.Status != domain.CodexAccountStatusValid {
		t.Fatalf("account = %#v", record.Snapshot)
	}
	accountDir := filepath.Join(root, testAccountID)
	for path, want := range map[string]os.FileMode{
		accountDir: 0o700,
		filepath.Join(accountDir, codexCredentialHomeDirectory):                          0o700,
		filepath.Join(accountDir, codexAccountDescriptorFilename):                        0o600,
		filepath.Join(accountDir, codexCredentialHomeDirectory, codexCredentialFilename): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(accountDir, codexAccountDescriptorFilename))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor map[string]any
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatal(err)
	}
	if len(descriptor) != 7 || descriptor["id"] != testAccountID || descriptor["accountEmail"] != email {
		t.Fatalf("descriptor = %s", data)
	}
	for _, forbidden := range []string{"credential", "token", "plan", "capacity", "usage", "authUrl"} {
		if _, exists := descriptor[forbidden]; exists {
			t.Errorf("descriptor contains forbidden field %q", forbidden)
		}
	}
}

func TestCodexAccountCatalogAllowsDuplicateEmailAndOrdersByCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	pending := filepath.Join(filepath.Dir(root), "pending-accounts")
	catalog := newCodexAccountCatalog(root, nil)
	ids := []string{testAccountID, "bb1e9a5d-37ad-43f8-83bd-13de8168f8af"}
	times := []time.Time{
		time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	}
	index := 0
	catalog.newID = func() string { return ids[index] }
	catalog.now = func() time.Time { return times[index] }
	email := "same@example.com"
	first := commitTestAccount(t, catalog, pending, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email})
	index++
	second := commitTestAccount(t, catalog, pending, "1c5de3ab-82d0-4a68-a06b-8495cdeab909", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email})
	if err := catalog.refresh(); err != nil {
		t.Fatal(err)
	}
	snapshots := catalog.snapshots()
	if len(snapshots) != 2 || snapshots[0].ID != second.Snapshot.ID || snapshots[1].ID != first.Snapshot.ID {
		t.Fatalf("ordered accounts = %#v", snapshots)
	}
	if err := os.RemoveAll(filepath.Join(root, second.Snapshot.ID)); err != nil {
		t.Fatal(err)
	}
	if err := catalog.refresh(); err != nil {
		t.Fatal(err)
	}
	if got := catalog.snapshots(); len(got) != 1 || got[0].ID != first.Snapshot.ID {
		t.Fatalf("accounts after rediscovery = %#v", got)
	}
}

func TestCodexAccountCatalogSurfacesUnsafeAndMalformedSlotsWithoutMetadataLeak(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	if err := ensurePrivateDirectory(root); err != nil {
		t.Fatal(err)
	}
	id := testAccountID
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	malformed := `{"version":1,"id":"` + id + `","source":"managed","authMethod":"chatgpt","accountEmail":"secret@example.com","createdAt":"2026-08-31T12:00:00Z","verifiedAt":"2026-08-31T12:00:00Z","token":"secret"}`
	if err := os.WriteFile(filepath.Join(dir, codexAccountDescriptorFilename), []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := newCodexAccountCatalog(root, nil)
	if err := catalog.refresh(); err != nil {
		t.Fatal(err)
	}
	got := catalog.snapshots()
	if len(got) != 1 || got[0].Status != domain.CodexAccountStatusBroken || got[0].ReasonCode != domain.CodexAccountReasonDescriptorInvalid {
		t.Fatalf("broken account = %#v", got)
	}
	if got[0].Label == "secret@example.com" || got[0].AccountEmail != nil {
		t.Fatalf("malformed descriptor metadata leaked: %#v", got[0])
	}
}

func TestCodexAccountCatalogRejectsSymlinkedCredentialHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "accounts")
	pending := filepath.Join(filepath.Dir(root), "pending-accounts")
	catalog := newCodexAccountCatalog(root, nil)
	catalog.newID = func() string { return testAccountID }
	record := commitTestAccount(t, catalog, pending, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodAPIKey})
	home := filepath.Join(root, record.Snapshot.ID, codexCredentialHomeDirectory)
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), home); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := catalog.refresh(); err != nil {
		t.Fatal(err)
	}
	broken, _ := catalog.record(record.Snapshot.ID)
	if broken.Snapshot.Status != domain.CodexAccountStatusBroken || broken.Snapshot.ReasonCode != domain.CodexAccountReasonUnsafePath {
		t.Fatalf("symlinked account = %#v", broken.Snapshot)
	}
}
