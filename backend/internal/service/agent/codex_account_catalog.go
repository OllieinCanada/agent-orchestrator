package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	codexAccountDescriptorFilename = "account.json"
	codexCredentialHomeDirectory   = "credential-home" //nolint:gosec // directory name, not a credential value.
	codexCredentialFilename        = "auth.json"
	codexAccountVersion            = 1
	maxCodexDescriptorBytes        = 16 << 10
)

type codexAccountDescriptor struct {
	Version      int                       `json:"version"`
	ID           string                    `json:"id"`
	Source       domain.CodexAccountSource `json:"source"`
	AuthMethod   domain.CodexAuthMethod    `json:"authMethod"`
	AccountEmail *string                   `json:"accountEmail,omitempty"`
	CreatedAt    time.Time                 `json:"createdAt"`
	VerifiedAt   time.Time                 `json:"verifiedAt"`
}

type codexAccountRecord struct {
	Snapshot  domain.CodexAccountSnapshot
	Home      string
	CreatedAt time.Time
}

type codexAccountCatalog struct {
	root   string
	logger *slog.Logger
	now    func() time.Time
	newID  func() string

	mu        sync.RWMutex
	records   map[string]codexAccountRecord
	onRemoved func([]string)
}

func newCodexAccountCatalog(root string, logger *slog.Logger) *codexAccountCatalog {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &codexAccountCatalog{
		root: root, logger: logger, now: func() time.Time { return time.Now().UTC() },
		newID: uuid.NewString, records: make(map[string]codexAccountRecord),
	}
}

func uncheckedAuthentication() domain.AgentAuthenticationObservation {
	return domain.AgentAuthenticationObservation{
		State: domain.AgentAuthenticationUnknown, Freshness: domain.AgentReadinessStale,
		ReasonCode: domain.AgentReadinessReasonNotChecked, Reason: "Authentication has not been checked yet.",
	}
}

func accountLabel(id string, method domain.CodexAuthMethod, email *string) string {
	if email != nil && safeAccountEmail(*email) {
		return strings.TrimSpace(*email)
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	switch method {
	case domain.CodexAuthMethodChatGPT:
		return "Codex ChatGPT account · " + short
	case domain.CodexAuthMethodAPIKey:
		return "Codex API key account · " + short
	default:
		return "Codex account · " + short
	}
}

func validAccountAuthMethod(method domain.CodexAuthMethod) bool {
	switch method {
	case domain.CodexAuthMethodChatGPT, domain.CodexAuthMethodAPIKey, domain.CodexAuthMethodOther, domain.CodexAuthMethodUnknown:
		return true
	default:
		return false
	}
}

func safeAccountEmail(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 254 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

func (c *codexAccountCatalog) snapshots() []domain.CodexAccountSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	records := c.sortedRecordsLocked()
	out := make([]domain.CodexAccountSnapshot, 0, len(records))
	for _, record := range records {
		out = append(out, record.Snapshot)
	}
	return out
}

func (c *codexAccountCatalog) recordsFor(ids []string) ([]codexAccountRecord, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(ids) == 0 {
		return c.sortedRecordsLocked(), nil
	}
	seen := make(map[string]struct{}, len(ids))
	records := make([]codexAccountRecord, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		record, ok := c.records[id]
		if !ok {
			return nil, unknownCodexAccountError{id: id}
		}
		seen[id] = struct{}{}
		records = append(records, record)
	}
	sortCodexAccountRecords(records)
	return records, nil
}

func (c *codexAccountCatalog) record(id string) (codexAccountRecord, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, ok := c.records[id]
	return record, ok
}

func (c *codexAccountCatalog) updateSnapshot(id string, update func(*domain.CodexAccountSnapshot)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.records[id]
	if !ok {
		return
	}
	update(&record.Snapshot)
	c.records[id] = record
}

func (c *codexAccountCatalog) updateVerifiedDescriptor(id string, observation ports.CodexAccountObservation) error {
	record, ok := c.record(id)
	if !ok || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return errors.New("codex account is unavailable")
	}
	descriptorPath := filepath.Join(c.root, id, codexAccountDescriptorFilename)
	descriptor, err := readCodexAccountDescriptor(descriptorPath)
	if err != nil {
		return err
	}
	descriptor.AuthMethod = observation.Method
	descriptor.AccountEmail = observation.Email
	descriptor.VerifiedAt = c.now()
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivateFileAtomic(descriptorPath, append(data, '\n')); err != nil {
		return err
	}
	c.updateSnapshot(id, func(snapshot *domain.CodexAccountSnapshot) {
		snapshot.AuthMethod = observation.Method
		snapshot.AccountEmail = observation.Email
		snapshot.Label = accountLabel(id, observation.Method, observation.Email)
	})
	return nil
}

func (c *codexAccountCatalog) sortedRecordsLocked() []codexAccountRecord {
	records := make([]codexAccountRecord, 0, len(c.records))
	for _, record := range c.records {
		records = append(records, record)
	}
	sortCodexAccountRecords(records)
	return records
}

func sortCodexAccountRecords(records []codexAccountRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.Before(records[j].CreatedAt)
		}
		return records[i].Snapshot.ID < records[j].Snapshot.ID
	})
}

func (c *codexAccountCatalog) refresh() error {
	if c.root == "" {
		return errors.New("codex account storage is unavailable")
	}
	if err := ensurePrivateDirectory(c.root); err != nil {
		return fmt.Errorf("prepare Codex account catalog: %w", err)
	}
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return fmt.Errorf("read Codex account catalog: %w", err)
	}
	next := make(map[string]codexAccountRecord)
	for _, entry := range entries {
		id := entry.Name()
		if !isCanonicalUUIDv4(id) {
			c.logger.Debug("ignored non-account Codex catalog entry")
			continue
		}
		record := c.readManaged(id)
		c.preserveObservation(&record)
		next[id] = record
	}
	c.replaceRecords(next)
	return nil
}

func (c *codexAccountCatalog) preserveObservation(record *codexAccountRecord) {
	c.mu.RLock()
	previous, ok := c.records[record.Snapshot.ID]
	c.mu.RUnlock()
	if !ok || previous.Snapshot.Status != domain.CodexAccountStatusValid || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return
	}
	record.Snapshot.Authentication = previous.Snapshot.Authentication
	record.Snapshot.Capacity = previous.Snapshot.Capacity
	record.Snapshot.UsageSummary = previous.Snapshot.UsageSummary
}

func (c *codexAccountCatalog) replaceRecords(records map[string]codexAccountRecord) {
	c.mu.Lock()
	removed := make([]string, 0)
	for id := range c.records {
		if _, ok := records[id]; !ok {
			removed = append(removed, id)
		}
	}
	c.records = records
	onRemoved := c.onRemoved
	c.mu.Unlock()
	if onRemoved != nil && len(removed) > 0 {
		onRemoved(removed)
	}
}

func (c *codexAccountCatalog) setOnRemoved(callback func([]string)) {
	c.mu.Lock()
	c.onRemoved = callback
	c.mu.Unlock()
}

func (c *codexAccountCatalog) readManaged(id string) codexAccountRecord {
	accountDir := filepath.Join(c.root, id)
	home := filepath.Join(accountDir, codexCredentialHomeDirectory)
	broken := func(code, reason string) codexAccountRecord {
		return codexAccountRecord{Home: home, Snapshot: domain.CodexAccountSnapshot{
			ID: id, Label: "Unavailable Codex account", Source: domain.CodexAccountSourceManaged,
			Status: domain.CodexAccountStatusBroken, ReasonCode: code, Reason: reason,
			Authentication: uncheckedAuthentication(), AuthMethod: domain.CodexAuthMethodUnknown,
			Capacity: unavailableCodexCapacity(),
		}}
	}
	info, err := os.Lstat(accountDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return broken(domain.CodexAccountReasonUnsafePath, "This Codex account has an unsafe directory layout.")
	}
	descriptor, err := readCodexAccountDescriptor(filepath.Join(accountDir, codexAccountDescriptorFilename))
	if err != nil || descriptor.ID != id || descriptor.Version != codexAccountVersion || descriptor.Source != domain.CodexAccountSourceManaged || !validAccountAuthMethod(descriptor.AuthMethod) || (descriptor.AccountEmail != nil && !safeAccountEmail(*descriptor.AccountEmail)) || descriptor.CreatedAt.IsZero() || descriptor.VerifiedAt.IsZero() {
		return broken(domain.CodexAccountReasonDescriptorInvalid, "This Codex account descriptor is invalid.")
	}
	homeInfo, err := os.Lstat(home)
	if errors.Is(err, os.ErrNotExist) {
		return broken(domain.CodexAccountReasonHomeMissing, "This Codex account credential home is missing.")
	}
	if err != nil || !homeInfo.IsDir() || homeInfo.Mode()&os.ModeSymlink != 0 || homeInfo.Mode().Perm() != 0o700 || !ownedByCurrentUser(homeInfo) || !pathWithin(c.root, home) {
		return broken(domain.CodexAccountReasonUnsafePath, "This Codex account has an unsafe credential home.")
	}
	credentialInfo, err := os.Lstat(filepath.Join(home, codexCredentialFilename))
	if err != nil || !credentialInfo.Mode().IsRegular() || credentialInfo.Mode()&os.ModeSymlink != 0 || credentialInfo.Mode().Perm() != 0o600 || credentialInfo.Size() <= 0 || credentialInfo.Size() > 8<<20 || !ownedByCurrentUser(credentialInfo) || !hasSingleHardLink(credentialInfo) {
		return broken(domain.CodexAccountReasonUnsafePath, "This Codex account credential is unavailable or unsafe.")
	}
	return codexAccountRecord{Home: canonicalPath(home), CreatedAt: descriptor.CreatedAt, Snapshot: domain.CodexAccountSnapshot{
		ID: id, Label: accountLabel(id, descriptor.AuthMethod, descriptor.AccountEmail),
		Source: domain.CodexAccountSourceManaged, Status: domain.CodexAccountStatusValid,
		ReasonCode: domain.CodexAccountReasonValid, Reason: "This Codex account is available.",
		Authentication: uncheckedAuthentication(), AuthMethod: descriptor.AuthMethod,
		AccountEmail: descriptor.AccountEmail, Capacity: uncheckedCodexCapacity(), CreatedAt: descriptor.CreatedAt,
	}}
}

func readCodexAccountDescriptor(path string) (codexAccountDescriptor, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() > maxCodexDescriptorBytes || !ownedByCurrentUser(info) || !hasSingleHardLink(info) {
		return codexAccountDescriptor{}, errors.New("descriptor is not a safe regular file")
	}
	f, err := os.Open(path) //nolint:gosec // AO-owned path verified with Lstat.
	if err != nil {
		return codexAccountDescriptor{}, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return codexAccountDescriptor{}, errors.New("descriptor changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCodexDescriptorBytes+1))
	if err != nil || len(data) > maxCodexDescriptorBytes || !utf8.Valid(data) {
		return codexAccountDescriptor{}, errors.New("descriptor is invalid")
	}
	var descriptor codexAccountDescriptor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&descriptor); err != nil {
		return codexAccountDescriptor{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return codexAccountDescriptor{}, errors.New("descriptor contains trailing data")
	}
	return descriptor, nil
}

func (c *codexAccountCatalog) commitPending(pendingDir string, observation ports.CodexAccountObservation) (codexAccountRecord, error) {
	if err := ensurePrivateDirectory(c.root); err != nil {
		return codexAccountRecord{}, err
	}
	id := c.newID()
	if !isCanonicalUUIDv4(id) {
		return codexAccountRecord{}, errors.New("generated invalid Codex account id")
	}
	createdAt := c.now()
	descriptor := codexAccountDescriptor{
		Version: codexAccountVersion, ID: id, Source: domain.CodexAccountSourceManaged,
		AuthMethod: observation.Method, AccountEmail: observation.Email,
		CreatedAt: createdAt, VerifiedAt: createdAt,
	}
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return codexAccountRecord{}, err
	}
	descriptorPath := filepath.Join(pendingDir, codexAccountDescriptorFilename)
	if err := writePrivateFileAtomic(descriptorPath, append(data, '\n')); err != nil {
		return codexAccountRecord{}, fmt.Errorf("write Codex account descriptor: %w", err)
	}
	target := filepath.Join(c.root, id)
	if err := os.Rename(pendingDir, target); err != nil {
		return codexAccountRecord{}, fmt.Errorf("commit Codex account: %w", err)
	}
	if err := syncDirectory(c.root); err != nil {
		return codexAccountRecord{}, err
	}
	record := c.readManaged(id)
	if record.Snapshot.Status != domain.CodexAccountStatusValid {
		return codexAccountRecord{}, errors.New("committed Codex account failed validation")
	}
	c.mu.Lock()
	c.records[id] = record
	c.mu.Unlock()
	return record, nil
}

func createPendingCredentialHome(pendingRoot, operationID string) (string, string, error) {
	if !isCanonicalUUIDv4(operationID) {
		return "", "", errors.New("invalid login operation id")
	}
	if err := ensurePrivateDirectory(pendingRoot); err != nil {
		return "", "", err
	}
	pendingDir := filepath.Join(pendingRoot, operationID)
	if err := os.Mkdir(pendingDir, 0o700); err != nil {
		return "", "", err
	}
	home := filepath.Join(pendingDir, codexCredentialHomeDirectory)
	if err := os.Mkdir(home, 0o700); err != nil {
		_ = os.RemoveAll(pendingDir)
		return "", "", err
	}
	return pendingDir, home, nil
}

func cleanupPendingCredentialHomes(pendingRoot string) error {
	if err := ensurePrivateDirectory(pendingRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(pendingRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(pendingRoot, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			if removeErr := os.Remove(path); removeErr != nil {
				return removeErr
			}
			continue
		}
		if !isCanonicalUUIDv4(entry.Name()) {
			// This is a private transient root, not the durable account catalog.
			// A crash may leave a non-UUID staging directory behind. Remove only
			// an owned, non-symlinked directory rooted directly below this private
			// parent; never follow an unsafe entry.
			if !ownedByCurrentUser(info) {
				return errors.New("unsafe Codex staging directory owner")
			}
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return removeErr
			}
			continue
		}
		if !ownedByCurrentUser(info) {
			return errors.New("unsafe Codex staging directory owner")
		}
		if removeErr := os.RemoveAll(path); removeErr != nil {
			return removeErr
		}
	}
	return syncDirectory(pendingRoot)
}

func ensurePrivateDirectory(path string) error {
	if path == "" {
		return errors.New("private directory path is empty")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return errors.New("path is not a safe directory")
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(path, 0o700) //nolint:gosec // directories must be owner-only and executable.
	}
	return nil
}

func writePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDirectory(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceCodexFile(tmpName, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // fixed AO-owned directory.
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

func pathWithin(root, candidate string) bool {
	root = canonicalPath(root)
	candidate = canonicalPath(candidate)
	if root == "" || candidate == "" {
		return false
	}
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isCanonicalUUIDv4(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 4 && parsed.String() == value
}

type unknownCodexAccountError struct{ id string }

func (e unknownCodexAccountError) Error() string { return "unknown Codex account id" }
