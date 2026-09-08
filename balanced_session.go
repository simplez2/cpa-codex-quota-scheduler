package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const balancedSessionLimit = 8192

type balancedSessionBinding struct {
	AuthID     string    `json:"auth_id"`
	AuthIndex  string    `json:"auth_index,omitempty"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// Use CPA's reconciled identity first. It covers body-based conversations and
// Codex forks that are not available in the plugin ABI's request headers.
// Raw prompts, authorization values and per-request IDs are never session keys.
func schedulerSessionHash(req pluginapi.SchedulerPickRequest) string {
	value := extractMetadataString(req.Options.Metadata, "canonical_session_id")
	if value == "" {
		sid := schedulerHeader(req.Options, "Session-Id")
		if sid == "" {
			sid = schedulerHeader(req.Options, "Session_id")
		}
		tid := schedulerHeader(req.Options, "Thread-Id")
		if tid == "" {
			tid = schedulerHeader(req.Options, "Thread_id")
		}
		raw := schedulerHeader(req.Options, "X-Codex-Turn-Metadata")
		if len(raw) <= 16<<10 {
			var meta struct {
				Session string `json:"session_id"`
				Thread  string `json:"thread_id"`
			}
			if json.Unmarshal([]byte(raw), &meta) == nil {
				if sid == "" {
					sid = strings.TrimSpace(meta.Session)
				}
				if tid == "" {
					tid = strings.TrimSpace(meta.Thread)
				}
			}
		}
		if tid != "" {
			value = tid
		} else {
			value = sid
		}
		if value == "" {
			value = schedulerHeader(req.Options, "X-Session-ID")
		}
		if value == "" {
			value = schedulerHeader(req.Options, "X-Session-Affinity")
		}
		if value == "" {
			value = extractMetadataString(req.Options.Metadata,
				"session_id", "sessionId", "thread_id", "threadId", "conversation_id", "conversationId",
				"prompt_cache_key", "derived_session_id", "execution_session_id", "executionSessionId")
		}
	}
	return scopedSchedulerSessionHash(req, value)
}

func schedulerParentSessionHash(req pluginapi.SchedulerPickRequest) string {
	// Only consume CPA's resolved parent relationship. Do not interpret request
	// IDs or invent a parent from text, account notes or another plugin.
	value := extractMetadataString(req.Options.Metadata, "parent_session_id")
	return scopedSchedulerSessionHash(req, value)
}

func scopedSchedulerSessionHash(req pluginapi.SchedulerPickRequest, value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	scope := extractMetadataString(req.Options.Metadata, "caller_scope")
	if len(scope) > 4096 {
		return ""
	}
	// Model changes stay on this conversation's account when CPA still offers
	// it. Caller scope prevents unrelated clients' equal IDs sharing a binding.
	raw, _ := json.Marshal([]string{providerCodex, scope, value})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func (s *schedulerRuntimeState) balancedSessionValidLocked(key string, binding balancedSessionBinding, now time.Time) bool {
	if s.cfg.StickySeconds <= 0 || binding.AuthID == "" || binding.LastUsedAt.IsZero() || now.Before(binding.LastUsedAt) {
		return false
	}
	if now.Sub(binding.LastUsedAt) <= time.Duration(s.cfg.StickySeconds)*time.Second {
		return true
	}
	// A long generation is active time, not idle time. Its completion renews
	// the binding so the following tool call does not start another account.
	if account := s.balancedAccounts[binding.AuthID]; account != nil {
		for _, pending := range account.Pending {
			if pending.Session == key && now.Sub(pending.At) < balancedPendingTTL {
				return true
			}
		}
	}
	return false
}

func (s *schedulerRuntimeState) balancedSessionLocked(key string, now time.Time) (balancedSessionBinding, bool) {
	binding, ok := s.balancedSessions[key]
	return binding, key != "" && ok && s.balancedSessionValidLocked(key, binding, now)
}

func (s *schedulerRuntimeState) pruneBalancedSessionsLocked(now time.Time) {
	if s.cfg.StickySeconds <= 0 {
		s.balancedSessions = nil
		return
	}
	if now.Sub(s.balancedSessionPrunedAt) < time.Minute && len(s.balancedSessions) < balancedSessionLimit {
		return
	}
	for key, binding := range s.balancedSessions {
		if !s.balancedSessionValidLocked(key, binding, now) {
			delete(s.balancedSessions, key)
		}
	}
	s.balancedSessionPrunedAt = now
}

func (s *schedulerRuntimeState) bindBalancedSessionLocked(key, authID, authIndex string, now time.Time) bool {
	previous, exists := s.balancedSessions[key]
	if s.balancedSessions == nil {
		s.balancedSessions = make(map[string]balancedSessionBinding)
	}
	if !exists && len(s.balancedSessions) >= balancedSessionLimit {
		oldestKey := ""
		oldest := now
		for k, b := range s.balancedSessions {
			if oldestKey == "" || b.LastUsedAt.Before(oldest) {
				oldestKey, oldest = k, b.LastUsedAt
			}
		}
		delete(s.balancedSessions, oldestKey)
	}
	valid := exists && s.balancedSessionValidLocked(key, previous, now)
	changed := !valid || previous.AuthID != authID || previous.AuthIndex != authIndex
	if valid && previous.AuthID != authID {
		s.balancedSessionSwitches++
	}
	s.balancedSessions[key] = balancedSessionBinding{AuthID: authID, AuthIndex: authIndex, LastUsedAt: now}
	return changed
}

func (s *schedulerRuntimeState) snapshotBalancedSessionsLocked(now time.Time) map[string]balancedSessionBinding {
	out := make(map[string]balancedSessionBinding)
	for key, binding := range s.balancedSessions {
		if len(out) >= balancedSessionLimit {
			break
		}
		if s.balancedSessionValidLocked(key, binding, now) {
			// Pending requests are process-local. Save a fresh lease when they
			// are the reason this long-running conversation is still active.
			if now.Sub(binding.LastUsedAt) > time.Duration(s.cfg.StickySeconds)*time.Second {
				binding.LastUsedAt = now
			}
			out[key] = binding
		}
	}
	return out
}

func (s *schedulerRuntimeState) restoreBalancedSessionsLocked(saved map[string]balancedSessionBinding, now time.Time) {
	if s.cfg.StickySeconds <= 0 {
		s.balancedSessions = nil
		return
	}
	if s.balancedSessions == nil {
		s.balancedSessions = make(map[string]balancedSessionBinding)
	}
	for key, binding := range saved {
		hash, err := hex.DecodeString(key)
		if err != nil || len(hash) != 16 || binding.AuthID == "" || !s.balancedSessionValidLocked(key, binding, now) {
			continue
		}
		previous, exists := s.balancedSessions[key]
		if exists && !binding.LastUsedAt.After(previous.LastUsedAt) {
			continue
		}
		if !exists && len(s.balancedSessions) >= balancedSessionLimit {
			continue
		}
		s.balancedSessions[key] = binding
	}
}

// Concurrent callers capture timestamps before acquiring the scheduler lock.
// Process them on a nondecreasing clock so an older queued request cannot
// expire or move a binding renewed by a request/completion that won the lock.
func (s *schedulerRuntimeState) balancedTimeLocked(now time.Time) time.Time {
	if now.Before(s.balancedClock) {
		return s.balancedClock
	}
	s.balancedClock = now
	return now
}
