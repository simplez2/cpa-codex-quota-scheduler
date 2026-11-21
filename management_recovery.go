package main

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// CPA's authenticated auth inventory supplies these fields, including model
// cooldowns. An absent/null list means the host did not report model details.
type cpaCooldown struct {
	Scope    string    `json:"scope"`
	ModelKey string    `json:"model_key,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	RetryAt  time.Time `json:"retry_at"`
}

type managementRecovery struct {
	NativeStatus string                      `json:"native_status"`
	CanClearAll  bool                        `json:"can_clear_local"`
	Accounts     []managementRecoveryAccount `json:"accounts"`
}

type managementRecoveryAccount struct {
	AuthID     string                     `json:"auth_id"`
	Plugin     *managementBanInfo         `json:"plugin,omitempty"`
	Guard      string                     `json:"guard,omitempty"`
	GuardUntil string                     `json:"guard_until,omitempty"`
	Quota      []managementRecoveryWindow `json:"quota,omitempty"`
	CPA        []cpaCooldown              `json:"cpa,omitempty"`
	CPAUnknown bool                       `json:"cpa_unavailable,omitempty"`
	CanClear   bool                       `json:"can_clear"`
}

type managementRecoveryWindow struct {
	Class      string `json:"class,omitempty"`
	State      string `json:"state"`
	ResetAt    string `json:"reset_at,omitempty"`
	ObservedAt string `json:"observed_at"`
	Cached     bool   `json:"cached,omitempty"`
}

func managementRecoveryWindows(q quotaSnapshot, now time.Time, staleAfter time.Duration) []managementRecoveryWindow {
	var exhausted []managementRecoveryWindow
	var restrictionAt time.Time
	for _, w := range q.Windows {
		if !quotaWindowHardRestriction(w) {
			continue
		}
		observedAt := w.ObservedAt
		if observedAt.IsZero() {
			observedAt = q.RefreshedAt
		}
		if observedAt.IsZero() || observedAt.After(now) {
			continue
		}
		elapsed := !w.ResetAt.IsZero() && !w.ResetAt.After(now)
		class := normalizeWindowClass(w.Class)
		if class == "" || w.UsedPercent < usedPercentThreshold {
			// Native allowed/limit_reached can apply to the whole account.
			// Do not attribute the restriction to a window with headroom.
			if !elapsed && observedAt.After(restrictionAt) {
				restrictionAt = observedAt
			}
			continue
		}
		item := managementRecoveryWindow{Class: class, State: "exhausted",
			ObservedAt: observedAt.Format(time.RFC3339), Cached: now.Sub(observedAt) > staleAfter}
		if !w.ResetAt.IsZero() {
			item.ResetAt = w.ResetAt.Format(time.RFC3339)
		}
		if elapsed {
			item.State = "awaiting_quota"
		}
		exhausted = append(exhausted, item)
	}
	if len(exhausted) == 0 && !restrictionAt.IsZero() {
		return []managementRecoveryWindow{{State: "restricted",
			ObservedAt: restrictionAt.Format(time.RFC3339), Cached: now.Sub(restrictionAt) > staleAfter}}
	}
	return exhausted
}

func (s *schedulerRuntimeState) managementRecovery(bans []managementBanInfo, now time.Time) managementRecovery {
	guards := s.recovery.snapshot()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return buildManagementRecovery(bans, guards, s.resetCreditInventory, s.quotas, s.recoveryInventoryAt, s.cfg, now)
}

func buildManagementRecovery(bans []managementBanInfo, guards map[string]recoveryEntry, inventory map[string]cpaAuthFileEntry, quotas map[string]quotaSnapshot, inventoryAt time.Time, cfg pluginConfig, now time.Time) managementRecovery {
	result := managementRecovery{NativeStatus: "stale", Accounts: []managementRecoveryAccount{}}
	// Inventory is normally refreshed at least every 30 seconds. Tolerate a
	// few missed ticks but do not label old host data as a current reading.
	fresh := !inventoryAt.IsZero() && !now.Before(inventoryAt) && now.Sub(inventoryAt) <= 2*time.Minute
	byID := make(map[string]*managementRecoveryAccount)
	row := func(id string) *managementRecoveryAccount {
		if byID[id] == nil {
			byID[id] = &managementRecoveryAccount{AuthID: id}
		}
		return byID[id]
	}
	pluginPending, probeRunning := false, false
	for i := range bans {
		ban := &bans[i]
		row(ban.AuthID).Plugin = ban
		pluginPending = true
		probeRunning = probeRunning || ban.State == string(banDispositionHalfOpen)
	}
	for id, guard := range guards {
		auth, found := inventory[id]
		if fresh && !found {
			continue
		}
		account := row(id)
		pluginPending = true
		switch {
		case guard.Owner != "":
			account.Guard = "half_open"
			probeRunning = true
		case fresh && auth.AuthIndex != guard.AuthIndex:
			// The guard still requires an observation for the replacement;
			// its previous binding's timer is no longer evidence.
			account.Guard = "awaiting_quota"
		case guard.RetryAt.After(now):
			account.Guard = "cooldown"
			account.GuardUntil = guard.RetryAt.Format(time.RFC3339)
		case guard.Ready:
			account.Guard = "probe_ready"
		default:
			account.Guard = "awaiting_quota"
			if guard.ResetAt.After(now) {
				account.GuardUntil = guard.ResetAt.Format(time.RFC3339)
			}
		}
	}
	if fresh {
		result.NativeStatus = "current"
	}
	for id, auth := range inventory {
		// Idle credentials use cached quota until a new observation. Neither
		// cache age nor the reset timer is evidence that the account recovered.
		if q, found := quotas[id]; found && q.AuthID == id && q.AuthIndex == auth.AuthIndex {
			if windows := managementRecoveryWindows(q, now, cfg.StaleAfter); len(windows) > 0 {
				row(id).Quota = windows
			}
		}
		if !fresh {
			continue
		}
		var cooldowns []cpaCooldown
		if json.Unmarshal(auth.Cooldowns, &cooldowns) != nil || cooldowns == nil {
			result.NativeStatus = "partial"
			// The older field can be a model aggregate. Display it with
			// unknown scope and without an action that clears all models.
			retryAt := parseQuotaTime(auth.NextRetryAfter)
			if auth.Unavailable || retryAt.After(now) {
				account := row(id)
				account.CPAUnknown = true
				if retryAt.After(now) {
					account.CPA = append(account.CPA, cpaCooldown{Scope: "unknown", RetryAt: retryAt})
				}
			}
			continue
		}
		for _, cooldown := range cooldowns {
			if cooldown.RetryAt.After(now) {
				account := row(id)
				account.CPA = append(account.CPA, cooldown)
			}
		}
	}
	result.CanClearAll = pluginPending && !probeRunning
	for id, account := range byID {
		probing := account.Guard == "half_open" || (account.Plugin != nil && account.Plugin.State == string(banDispositionHalfOpen))
		account.CanClear = account.Plugin != nil && !probing
		if fresh && len(account.CPA) > 0 && !account.CPAUnknown {
			onlyQuota := true
			for _, entry := range account.CPA {
				onlyQuota = onlyQuota && (entry.Reason == "quota" || entry.Reason == "credential_quota") &&
					(entry.Scope == "credential" || (entry.Scope == "model" && strings.TrimSpace(entry.ModelKey) != ""))
			}
			auth, q := inventory[id], quotas[id]
			eval := quotaEvaluationWithOrder(q, now, cfg.WindowOrder)
			account.CanClear = !probing && onlyQuota && auth.AuthIndex != "" && auth.AuthIndex == q.AuthIndex &&
				quotaSnapshotFresh(q, now, cfg.StaleAfter) && eval.Known && eval.Eligible
		}
		if account.CPAUnknown || len(account.Quota) > 0 {
			account.CanClear = false
		}
		result.Accounts = append(result.Accounts, *account)
	}
	sort.Slice(result.Accounts, func(i, j int) bool { return result.Accounts[i].AuthID < result.Accounts[j].AuthID })
	return result
}
