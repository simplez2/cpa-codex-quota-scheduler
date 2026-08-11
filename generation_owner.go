package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	schedulerGenerationRecordVersion = 1
	generationLockWait               = 2 * time.Second
	generationLockRetry              = 5 * time.Millisecond
	generationRecordMaxBytes         = 1 << 20
)

var errGenerationLockBusy = errors.New("scheduler generation record is busy")

// schedulerGenerationRecord is an append-only cross-DSO ownership journal.
// Reservations do not replace the active owner until the new instance has a
// complete Keeper snapshot ready to publish.
type schedulerGenerationRecord struct {
	Version          int       `json:"version"`
	HighestReserved  uint64    `json:"highest_reserved"`
	ActiveGeneration uint64    `json:"active_generation"`
	ActiveOwner      string    `json:"active_owner,omitempty"`
	Active           bool      `json:"active"`
	ReservedAt       time.Time `json:"reserved_at,omitempty"`
	ClaimedAt        time.Time `json:"claimed_at,omitempty"`
	ReleasedAt       time.Time `json:"released_at,omitempty"`
}

type schedulerGenerationOwnership struct {
	Managed            bool
	StatePath          string
	Owner              string
	Ticket             uint64
	Claimed            bool
	ClaimedAt          time.Time
	Released           bool
	Superseded         bool
	ObservedGeneration uint64
	SupersedeReason    string
}

type schedulerGenerationStatus struct {
	Managed            bool
	Ticket             uint64
	Claimed            bool
	Active             bool
	Released           bool
	Superseded         bool
	ObservedGeneration uint64
	OwnerFingerprint   string
	ClaimedAt          time.Time
	SupersedeReason    string
}

func (s *schedulerRuntimeState) resetGenerationOwnership() {
	s.generationMu.Lock()
	s.generation = schedulerGenerationOwnership{}
	s.generationMu.Unlock()
}

func (s *schedulerRuntimeState) initializeGenerationOwnership(statePath string) {
	statePath = strings.TrimSpace(statePath)
	s.generationMu.Lock()
	s.generation = schedulerGenerationOwnership{
		Managed:   statePath != "",
		StatePath: statePath,
	}
	s.generationMu.Unlock()
}

// reserveGenerationOwnership allocates a monotonic ticket without replacing
// the current owner. The ticket is promoted only after the first successful
// Keeper refresh. state_path="" preserves legacy and unit-test behavior.
func (s *schedulerRuntimeState) reserveGenerationOwnership(statePath string) error {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		s.initializeGenerationOwnership("")
		return nil
	}
	owner, err := newSchedulerGenerationOwner()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var ticket uint64
	_, err = updateSchedulerGenerationRecord(statePath, func(record *schedulerGenerationRecord) (bool, error) {
		ticket = record.HighestReserved
		if record.ActiveGeneration > ticket {
			ticket = record.ActiveGeneration
		}
		if ticket == ^uint64(0) {
			return false, errors.New("scheduler generation exhausted")
		}
		ticket++
		record.HighestReserved = ticket
		record.ReservedAt = now
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("reserve scheduler generation: %w", err)
	}
	s.generationMu.Lock()
	s.generation = schedulerGenerationOwnership{
		Managed:   true,
		StatePath: statePath,
		Owner:     owner,
		Ticket:    ticket,
	}
	s.generationMu.Unlock()
	return nil
}

// generationCanRefresh permits the initial refresh for the newest reserved
// instance and verifies ownership for an already claimed instance.
func (s *schedulerRuntimeState) generationCanRefresh() bool {
	ownership := s.generationSnapshot()
	if !ownership.Managed {
		return s.runtimeStatePath() == ""
	}
	if ownership.Superseded || ownership.Released {
		return false
	}
	if ownership.Claimed {
		return s.generationOwnerActive()
	}
	record, err := readSchedulerGenerationRecord(ownership.StatePath)
	if err != nil {
		slog.Warn("codex-quota-scheduler: generation check failed before refresh", "error", err)
		return false
	}
	if record.HighestReserved > ownership.Ticket || record.ActiveGeneration >= ownership.Ticket {
		s.markGenerationSuperseded(record.ActiveGeneration, "newer_generation_before_first_refresh")
		return false
	}
	if record.HighestReserved != ownership.Ticket {
		slog.Warn("codex-quota-scheduler: reserved generation record is inconsistent",
			"ticket", ownership.Ticket,
			"highest_reserved", record.HighestReserved)
		return false
	}
	return true
}

// claimGenerationAfterSuccessfulRefresh promotes only the newest reservation.
// Older reservations permanently retire and cannot claim after a tombstone.
func (s *schedulerRuntimeState) claimGenerationAfterSuccessfulRefresh() (claimedNow, active bool, err error) {
	ownership := s.generationSnapshot()
	if !ownership.Managed {
		return false, true, nil
	}
	if ownership.Superseded || ownership.Released {
		return false, false, nil
	}
	if ownership.Claimed {
		return false, s.generationOwnerActive(), nil
	}
	now := time.Now().UTC()
	superseded := false
	observed := uint64(0)
	record, err := updateSchedulerGenerationRecord(ownership.StatePath, func(record *schedulerGenerationRecord) (bool, error) {
		observed = record.ActiveGeneration
		if record.HighestReserved > ownership.Ticket || record.ActiveGeneration >= ownership.Ticket {
			superseded = true
			return false, nil
		}
		if record.HighestReserved != ownership.Ticket {
			return false, fmt.Errorf("generation ticket %d is not newest reservation %d", ownership.Ticket, record.HighestReserved)
		}
		record.ActiveGeneration = ownership.Ticket
		record.ActiveOwner = ownership.Owner
		record.Active = true
		record.ClaimedAt = now
		record.ReleasedAt = time.Time{}
		return true, nil
	})
	if err != nil {
		return false, false, fmt.Errorf("claim scheduler generation: %w", err)
	}
	if superseded {
		s.markGenerationSuperseded(observed, "newer_generation_claimed_first")
		return false, false, nil
	}
	if record.ActiveGeneration != ownership.Ticket || record.ActiveOwner != ownership.Owner || !record.Active {
		s.markGenerationSuperseded(record.ActiveGeneration, "generation_claim_verification_failed")
		return false, false, nil
	}
	// The durable fence now prevents the retiring owner from committing another
	// shared-state snapshot. Reload before publishing Claimed=true in memory, so
	// incoming usage callbacks cannot persist a pre-fence reset confirmation and
	// resurrect evidence the previous owner already deleted. Warmup outcomes keep
	// their separate cross-DSO journal/merge path.
	s.loadBanStateAfterGenerationClaim(ownership.StatePath)
	s.generationMu.Lock()
	if !s.generation.Superseded && s.generation.Ticket == ownership.Ticket && s.generation.Owner == ownership.Owner {
		s.generation.Claimed = true
		s.generation.ClaimedAt = now
		claimedNow = true
	}
	s.generationMu.Unlock()
	return claimedNow, claimedNow, nil
}

// generationOwnerActive is the fail-closed gate for background writers. Any
// durable mismatch after a claim permanently supersedes this loaded DSO.
func (s *schedulerRuntimeState) generationOwnerActive() bool {
	ownership := s.generationSnapshot()
	if !ownership.Managed {
		return s.runtimeStatePath() == "" && !ownership.Superseded
	}
	if ownership.Superseded || ownership.Released || !ownership.Claimed {
		return false
	}
	record, err := readSchedulerGenerationRecord(ownership.StatePath)
	if err != nil {
		slog.Warn("codex-quota-scheduler: generation owner verification failed", "error", err)
		return false
	}
	if record.Active && record.ActiveGeneration == ownership.Ticket && record.ActiveOwner == ownership.Owner {
		return true
	}
	s.markGenerationSuperseded(record.ActiveGeneration, "generation_owner_mismatch")
	return false
}

// withGenerationOwnerCommit verifies the owner and runs the shared-state
// commit while holding the same inter-process generation lock used by reserve,
// claim, and release. This closes the check-then-rename takeover race.
func (s *schedulerRuntimeState) withGenerationOwnerCommit(commit func() error) (bool, error) {
	ownership := s.generationSnapshot()
	if !ownership.Managed {
		if s.runtimeStatePath() != "" || ownership.Superseded {
			return false, nil
		}
		return true, commit()
	}
	if ownership.Superseded || ownership.Released || !ownership.Claimed {
		return false, nil
	}
	mismatch := false
	observed := uint64(0)
	_, err := updateSchedulerGenerationRecord(ownership.StatePath, func(record *schedulerGenerationRecord) (bool, error) {
		observed = record.ActiveGeneration
		if !record.Active || record.ActiveGeneration != ownership.Ticket || record.ActiveOwner != ownership.Owner {
			mismatch = true
			return false, nil
		}
		return false, commit()
	})
	if err != nil {
		return false, err
	}
	if mismatch {
		s.markGenerationSuperseded(observed, "generation_commit_owner_mismatch")
		return false, nil
	}
	return true, nil
}

func (s *schedulerRuntimeState) runtimeStatePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.TrimSpace(s.cfg.StatePath)
}

// releaseGenerationOwnership appends a tombstone instead of deleting or
// decrementing the generation, so a retired slower instance cannot revive.
func (s *schedulerRuntimeState) releaseGenerationOwnership() error {
	ownership := s.generationSnapshot()
	if !ownership.Managed || ownership.Superseded || ownership.Released || !ownership.Claimed {
		return nil
	}
	now := time.Now().UTC()
	mismatch := false
	observed := uint64(0)
	_, err := updateSchedulerGenerationRecord(ownership.StatePath, func(record *schedulerGenerationRecord) (bool, error) {
		observed = record.ActiveGeneration
		if !record.Active || record.ActiveGeneration != ownership.Ticket || record.ActiveOwner != ownership.Owner {
			mismatch = true
			return false, nil
		}
		record.Active = false
		record.ReleasedAt = now
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("release scheduler generation: %w", err)
	}
	if mismatch {
		s.markGenerationSuperseded(observed, "generation_release_owner_mismatch")
		return nil
	}
	s.generationMu.Lock()
	if s.generation.Ticket == ownership.Ticket && s.generation.Owner == ownership.Owner {
		s.generation.Released = true
	}
	s.generationMu.Unlock()
	return nil
}

func (s *schedulerRuntimeState) generationSnapshot() schedulerGenerationOwnership {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()
	return s.generation
}

func (s *schedulerRuntimeState) generationStatus() schedulerGenerationStatus {
	ownership := s.generationSnapshot()
	status := schedulerGenerationStatus{
		Managed:            ownership.Managed,
		Ticket:             ownership.Ticket,
		Claimed:            ownership.Claimed,
		Released:           ownership.Released,
		Superseded:         ownership.Superseded,
		ObservedGeneration: ownership.ObservedGeneration,
		OwnerFingerprint:   schedulerGenerationOwnerFingerprint(ownership.Owner),
		ClaimedAt:          ownership.ClaimedAt,
		SupersedeReason:    ownership.SupersedeReason,
	}
	if ownership.Managed && ownership.Claimed && !ownership.Released && !ownership.Superseded {
		status.Active = s.generationOwnerActive()
	} else if !ownership.Managed {
		// Legacy state_path="" runtimes remain active without a generation
		// journal. A configured but unmanaged runtime (for example, failed
		// initialization or disabled startup) must be reported inactive.
		status.Active = s.runtimeStatePath() == "" && !ownership.Superseded
	}
	return status
}

func (s *schedulerRuntimeState) markGenerationSuperseded(observed uint64, reason string) {
	s.generationMu.Lock()
	if !s.generation.Managed || s.generation.Superseded {
		s.generationMu.Unlock()
		return
	}
	s.generation.Superseded = true
	if observed > s.generation.ObservedGeneration {
		s.generation.ObservedGeneration = observed
	}
	s.generation.SupersedeReason = strings.TrimSpace(reason)
	ticket := s.generation.Ticket
	owner := schedulerGenerationOwnerFingerprint(s.generation.Owner)
	s.generationMu.Unlock()

	s.mu.Lock()
	s.stopping = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	slog.Info("codex-quota-scheduler: retired generation permanently superseded",
		"generation", ticket,
		"observed_generation", observed,
		"owner", owner,
		"reason", reason)
}

func newSchedulerGenerationOwner() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate scheduler generation owner: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func schedulerGenerationOwnerFingerprint(owner string) string {
	owner = strings.TrimSpace(owner)
	if len(owner) > 10 {
		return owner[:10]
	}
	return owner
}

func schedulerGenerationPath(statePath string) string {
	return strings.TrimSpace(statePath) + ".generation"
}

func readSchedulerGenerationRecord(statePath string) (schedulerGenerationRecord, error) {
	return updateSchedulerGenerationRecord(statePath, func(_ *schedulerGenerationRecord) (bool, error) {
		return false, nil
	})
}

// updateSchedulerGenerationRecord locks an append-only journal. A previous
// complete record survives a host crash during the next append.
func updateSchedulerGenerationRecord(statePath string, update func(*schedulerGenerationRecord) (bool, error)) (schedulerGenerationRecord, error) {
	path := schedulerGenerationPath(statePath)
	if strings.TrimSpace(statePath) == "" {
		return schedulerGenerationRecord{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return schedulerGenerationRecord{}, fmt.Errorf("create generation directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return schedulerGenerationRecord{}, fmt.Errorf("open generation record: %w", err)
	}
	locked := false
	deadline := time.Now().Add(generationLockWait)
	for {
		locked, err = tryExclusiveFileLock(file)
		if err != nil || locked || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(generationLockRetry)
	}
	if err != nil {
		_ = file.Close()
		return schedulerGenerationRecord{}, fmt.Errorf("lock generation record: %w", err)
	}
	if !locked {
		_ = file.Close()
		return schedulerGenerationRecord{}, errGenerationLockBusy
	}
	defer func() {
		_ = unlockExclusiveFile(file)
		_ = file.Close()
	}()

	record, err := readSchedulerGenerationRecordLocked(file)
	if err != nil {
		return schedulerGenerationRecord{}, err
	}
	write, err := update(&record)
	if err != nil {
		return schedulerGenerationRecord{}, err
	}
	if write {
		record.Version = schedulerGenerationRecordVersion
		if err := validateSchedulerGenerationRecord(record); err != nil {
			return schedulerGenerationRecord{}, err
		}
		if err := appendSchedulerGenerationRecordLocked(file, record); err != nil {
			return schedulerGenerationRecord{}, err
		}
	}
	return record, nil
}

func readSchedulerGenerationRecordLocked(file *os.File) (schedulerGenerationRecord, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return schedulerGenerationRecord{}, err
	}
	stat, err := file.Stat()
	if err != nil {
		return schedulerGenerationRecord{}, err
	}
	if stat.Size() == 0 {
		return schedulerGenerationRecord{Version: schedulerGenerationRecordVersion}, nil
	}
	if stat.Size() > generationRecordMaxBytes {
		return schedulerGenerationRecord{}, fmt.Errorf("generation journal exceeds %d bytes", generationRecordMaxBytes)
	}
	scanner := bufio.NewScanner(io.LimitReader(file, generationRecordMaxBytes+1))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	var latest schedulerGenerationRecord
	found := false
	nonEmpty := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		nonEmpty = true
		var candidate schedulerGenerationRecord
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			continue
		}
		if err := validateSchedulerGenerationRecord(candidate); err != nil {
			continue
		}
		latest = candidate
		found = true
	}
	if err := scanner.Err(); err != nil {
		return schedulerGenerationRecord{}, err
	}
	if nonEmpty && !found {
		return schedulerGenerationRecord{}, errors.New("generation journal has no valid record")
	}
	if !found {
		latest.Version = schedulerGenerationRecordVersion
	}
	return latest, nil
}

func appendSchedulerGenerationRecordLocked(file *os.File, record schedulerGenerationRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	stat, err := file.Stat()
	if err != nil {
		return err
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
	if stat.Size()+int64(len(separator)+len(raw)) > generationRecordMaxBytes {
		return fmt.Errorf("generation journal exceeds %d bytes", generationRecordMaxBytes)
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

func validateSchedulerGenerationRecord(record schedulerGenerationRecord) error {
	if record.Version != 0 && record.Version != schedulerGenerationRecordVersion {
		return fmt.Errorf("unsupported generation record version %d", record.Version)
	}
	if record.ActiveGeneration > record.HighestReserved {
		return errors.New("active generation exceeds highest reservation")
	}
	if record.Active && (record.ActiveGeneration == 0 || strings.TrimSpace(record.ActiveOwner) == "") {
		return errors.New("active generation has no owner")
	}
	return nil
}
