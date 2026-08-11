package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const warmupInstanceLeaseTTL = 2 * warmupRequestTimeout

type warmupInstanceLeaseRecord struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type warmupInstanceLease struct {
	file        *os.File
	statePath   string
	owner       string
	releaseOnce sync.Once
	releaseErr  error
}

const (
	warmupOutcomeJournalVersion  = 1
	warmupOutcomeJournalMaxBytes = 4 << 20
)

type warmupOutcomeJournalRecord struct {
	Version    int         `json:"version"`
	Key        string      `json:"key"`
	Entry      warmupEntry `json:"entry"`
	Ban        *banEntry   `json:"ban,omitempty"`
	RecordedAt time.Time   `json:"recorded_at"`
}

// acquireWarmupInstanceLease serializes warmup across independently loaded
// plugin instances that share the same state_path. The OS lock is authoritative:
// it is released automatically if CPA crashes or unloads the owning plugin, so
// stale JSON metadata can never permanently block a later instance.
func acquireWarmupInstanceLease(statePath string, now time.Time) (*warmupInstanceLease, bool, error) {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return &warmupInstanceLease{}, true, nil
	}
	lockPath := statePath + ".warmup.lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, false, fmt.Errorf("create warmup lease directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, fmt.Errorf("open warmup lease: %w", err)
	}
	locked, err := tryExclusiveFileLock(file)
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("lock warmup lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, false, nil
	}
	owner, err := newWarmupInstanceLeaseOwner()
	if err != nil {
		_ = unlockExclusiveFile(file)
		_ = file.Close()
		return nil, false, err
	}
	lease := &warmupInstanceLease{file: file, statePath: statePath, owner: owner}
	if err := writeWarmupInstanceLeaseRecord(file, warmupInstanceLeaseRecord{
		Owner:     owner,
		ExpiresAt: now.Add(warmupInstanceLeaseTTL).UTC(),
	}); err != nil {
		_ = lease.release()
		return nil, false, fmt.Errorf("write warmup lease: %w", err)
	}
	return lease, true, nil
}

// mergePersistedWarmupsLocked refreshes this instance after it wins the
// cross-instance lease. This closes the hot-reload race where another plugin
// instance finishes and persists a warmup after this instance loaded its
// initial state but before its next refresh. The caller must hold warmupMu.
func (s *schedulerRuntimeState) mergePersistedWarmupsLocked(statePath string) (map[string]banEntry, bool, error) {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return nil, false, nil
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, false, fmt.Errorf("read persisted scheduler state: %w", err)
		}
	} else {
		var state struct {
			Warmups map[string]warmupEntry `json:"warmups"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, false, fmt.Errorf("decode persisted scheduler state: %w", err)
		}
		s.mergeWarmupEntriesLocked(state.Warmups)
	}
	records, err := readWarmupOutcomeJournal(statePath)
	if err != nil {
		return nil, false, err
	}
	bans := make(map[string]banEntry)
	for _, record := range records {
		s.mergeWarmupEntriesLocked(map[string]warmupEntry{record.Key: record.Entry})
		if record.Ban != nil && strings.TrimSpace(record.Entry.AuthID) != "" {
			bans[record.Entry.AuthID] = *record.Ban
		}
	}
	return bans, len(records) > 0, nil
}

func (s *schedulerRuntimeState) mergeWarmupEntriesLocked(entries map[string]warmupEntry) {
	if s.warmups == nil {
		s.warmups = make(map[string]warmupEntry)
	}
	for key, incoming := range entries {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(incoming.AuthID) == "" || incoming.AttemptedAt.IsZero() {
			continue
		}
		current, exists := s.warmups[key]
		if !exists || warmupEntryNewer(incoming, current) {
			s.warmups[key] = incoming
		}
	}
}

func warmupEntryNewer(incoming, current warmupEntry) bool {
	incomingAt := warmupEntryRevisionTime(incoming)
	currentAt := warmupEntryRevisionTime(current)
	if !incomingAt.Equal(currentAt) {
		return incomingAt.After(currentAt)
	}
	if incoming.Blocked != current.Blocked {
		return incoming.Blocked
	}
	if incoming.Status != current.Status {
		return incoming.Status != 0
	}
	if incoming.Error != current.Error {
		return incoming.Error != ""
	}
	return incoming.ResetAt.After(current.ResetAt)
}

func warmupEntryRevisionTime(entry warmupEntry) time.Time {
	latest := entry.AttemptedAt
	for _, candidate := range []time.Time{entry.CompletedAt, entry.ActivatedAt} {
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

// persistWarmupLeaseOutcome is the only write a superseded instance may make.
// It records only the outcome of the request that already owned the cross-DSO
// warmup lease; it never writes the full scheduler state. The next generation
// merges this journal before deciding whether another activation is needed.
func (s *schedulerRuntimeState) persistWarmupLeaseOutcome(lease *warmupInstanceLease, candidate warmupCandidate) error {
	if lease == nil || lease.file == nil || strings.TrimSpace(lease.statePath) == "" {
		return nil
	}
	key := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
	s.warmupMu.Lock()
	entry, ok := s.warmups[key]
	s.warmupMu.Unlock()
	if !ok || entry.AttemptedAt.IsZero() {
		return nil
	}
	record := warmupOutcomeJournalRecord{
		Version:    warmupOutcomeJournalVersion,
		Key:        key,
		Entry:      entry,
		RecordedAt: time.Now().UTC(),
	}
	if ban, found := banStore.lookup(candidate.Snapshot.AuthID); found &&
		!ban.BannedAt.Before(entry.AttemptedAt.Add(-time.Second)) {
		copy := ban
		record.Ban = &copy
	}
	return appendWarmupOutcomeJournal(lease.statePath, record)
}

func warmupOutcomeJournalPath(statePath string) string {
	return strings.TrimSpace(statePath) + ".warmup.outcomes"
}

func appendWarmupOutcomeJournal(statePath string, record warmupOutcomeJournalRecord) error {
	path := warmupOutcomeJournalPath(statePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > warmupOutcomeJournalMaxBytes {
		return fmt.Errorf("warmup outcome journal exceeds %d bytes", warmupOutcomeJournalMaxBytes)
	}
	separator := []byte(nil)
	if stat.Size() > 0 {
		if _, err := file.Seek(-1, io.SeekEnd); err != nil {
			return err
		}
		last := []byte{0}
		if _, err := io.ReadFull(file, last); err != nil {
			return err
		}
		if last[0] != '\n' {
			separator = []byte{'\n'}
		}
	}
	if stat.Size()+int64(len(separator)+len(raw)) > warmupOutcomeJournalMaxBytes {
		return fmt.Errorf("warmup outcome journal exceeds %d bytes", warmupOutcomeJournalMaxBytes)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := writeFileAll(file, separator); err != nil {
		return err
	}
	if err := writeFileAll(file, raw); err != nil {
		return err
	}
	return file.Sync()
}

func clearWarmupOutcomeJournal(statePath string) error {
	path := warmupOutcomeJournalPath(statePath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeFileAll(file *os.File, raw []byte) error {
	for len(raw) > 0 {
		written, err := file.Write(raw)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		raw = raw[written:]
	}
	return nil
}

func readWarmupOutcomeJournal(statePath string) ([]warmupOutcomeJournalRecord, error) {
	path := warmupOutcomeJournalPath(statePath)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > warmupOutcomeJournalMaxBytes {
		return nil, fmt.Errorf("warmup outcome journal exceeds %d bytes", warmupOutcomeJournalMaxBytes)
	}
	scanner := bufio.NewScanner(io.LimitReader(file, warmupOutcomeJournalMaxBytes+1))
	scanner.Buffer(make([]byte, 4096), 256*1024)
	records := make([]warmupOutcomeJournalRecord, 0)
	sawNonEmpty := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		sawNonEmpty = true
		var record warmupOutcomeJournalRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if record.Version != warmupOutcomeJournalVersion || strings.TrimSpace(record.Key) == "" || record.Entry.AttemptedAt.IsZero() {
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if sawNonEmpty && len(records) == 0 {
		return nil, errors.New("warmup outcome journal has no valid record")
	}
	return records, nil
}

func newWarmupInstanceLeaseOwner() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate warmup instance owner: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeWarmupInstanceLeaseRecord(file *os.File, record warmupInstanceLeaseRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if err := writeFileAll(file, raw); err != nil {
		return err
	}
	return file.Sync()
}

func (l *warmupInstanceLease) release() error {
	if l == nil {
		return nil
	}
	l.releaseOnce.Do(func() {
		if l.file == nil {
			return
		}
		metadataErr := writeWarmupInstanceLeaseRecord(l.file, warmupInstanceLeaseRecord{
			Owner:     l.owner,
			ExpiresAt: time.Now().UTC(),
		})
		unlockErr := unlockExclusiveFile(l.file)
		closeErr := l.file.Close()
		l.file = nil
		l.releaseErr = errors.Join(metadataErr, unlockErr, closeErr)
	})
	return l.releaseErr
}
