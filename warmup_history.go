package main

import "time"

const warmupHistoryLimit = 256

// Keep user-visible outcomes independently from retry/deduplication state.
// Caller owns warmupMu. One generation attempt is recorded once, even when it
// jointly starts weekly and 5h windows.
func (s *schedulerRuntimeState) rememberWarmupLocked(entry warmupEntry, create bool, now time.Time) {
	if entry.AttemptedAt.IsZero() || now.Sub(entry.AttemptedAt) > 7*24*time.Hour {
		return
	}
	kept := s.warmupHistory[:0]
	for _, previous := range s.warmupHistory {
		if now.Sub(previous.AttemptedAt) <= 7*24*time.Hour {
			kept = append(kept, previous)
		}
	}
	s.warmupHistory = kept
	for i, previous := range s.warmupHistory {
		if previous.AuthID == entry.AuthID && previous.AuthIndex == entry.AuthIndex && previous.AttemptedAt.Equal(entry.AttemptedAt) {
			if previous.Window == entry.Window {
				s.warmupHistory[i] = entry
			}
			return
		}
	}
	if !create {
		return
	}
	s.warmupHistory = append(s.warmupHistory, entry)
	if len(s.warmupHistory) > warmupHistoryLimit {
		s.warmupHistory = append([]warmupEntry(nil), s.warmupHistory[len(s.warmupHistory)-warmupHistoryLimit:]...)
	}
}
