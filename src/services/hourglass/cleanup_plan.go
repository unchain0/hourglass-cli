package hourglass

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CleanupRules explicitly defines the safe roster and slots the cleanup planner may use.
type CleanupRules struct {
	EligibleUserIDs               []int    `json:"eligible_user_ids"`
	Types                         []string `json:"types"`
	MaxAssignmentsPerUserPerMonth int      `json:"max_assignments_per_user_per_month"`
	DisallowSameDate              bool     `json:"disallow_same_date"`
}

// CleanupMutation is one existing AV slot assigned by a cleanup preview.
type CleanupMutation struct {
	SlotID       int    `json:"slot_id"`
	Date         string `json:"date"`
	Type         string `json:"type"`
	Slot         int    `json:"slot"`
	AssigneeID   int    `json:"assignee_id"`
	AssigneeName string `json:"assignee_name"`
	UpdatedAt    string `json:"updated_at"`
}

// CleanupSkipped explains why a target-month AV row is unchanged.
type CleanupSkipped struct {
	SlotID int    `json:"slot_id"`
	Date   string `json:"date"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// CleanupPlan is a deterministic, preview-first cleanup rotation plan.
type CleanupPlan struct {
	DryRun      bool              `json:"dry_run"`
	Month       string            `json:"month"`
	HistoryFrom string            `json:"history_from"`
	HistoryTo   string            `json:"history_to"`
	LGroup      int               `json:"lgroup"`
	RulesDigest string            `json:"rules_digest"`
	Mutations   []CleanupMutation `json:"mutations"`
	Skipped     []CleanupSkipped  `json:"skipped"`
	Confirm     string            `json:"confirm,omitempty"`
}

// BuildCleanupPlan assigns only vacant, unsent, unconfirmed, non-extra AV rows from explicit rules.
func BuildCleanupPlan(month time.Time, history, target []AVAttendant, users []User, rules CleanupRules, historyMonths, lgroup int) (CleanupPlan, error) {
	if err := validateCleanupRules(rules); err != nil {
		return CleanupPlan{}, err
	}
	if historyMonths < 1 {
		return CleanupPlan{}, fmt.Errorf("history months must be at least 1")
	}

	plan := newCleanupPlan(month, rules, historyMonths, lgroup)
	allowed := stringSet(rules.Types)
	eligible := intSet(rules.EligibleUserIDs)
	planner := cleanupPlanner{
		rules:      rules,
		allowed:    allowed,
		names:      userDisplayNames(users),
		historical: countEligible(history, allowed, eligible),
	}
	planner.monthCounts, planner.datesByUser = monthlyUsage(target, allowed, eligible)
	for _, row := range sortedTarget(target) {
		planner.applyRow(&plan, row)
	}
	plan.Confirm = CleanupPlanConfirmToken(plan)
	return plan, nil
}

func newCleanupPlan(month time.Time, rules CleanupRules, historyMonths, lgroup int) CleanupPlan {
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	historyFrom := month.AddDate(0, -historyMonths, 0)
	return CleanupPlan{
		DryRun:      true,
		Month:       month.Format("2006-01"),
		HistoryFrom: historyFrom.Format("2006-01-02"),
		HistoryTo:   month.AddDate(0, 0, -1).Format("2006-01-02"),
		LGroup:      lgroup,
		RulesDigest: CleanupRulesDigest(rules),
	}
}

func userDisplayNames(users []User) map[int]string {
	index := mapUsers(users)
	names := make(map[int]string, len(users))
	for _, user := range users {
		names[user.ID] = LookupUserName(index, user.ID)
	}
	return names
}

func countEligible(rows []AVAttendant, allowed map[string]bool, eligible map[int]bool) map[int]int {
	counts := make(map[int]int)
	for _, row := range rows {
		if row.Assignee != nil && allowed[row.Type] && eligible[*row.Assignee] {
			counts[*row.Assignee]++
		}
	}
	return counts
}

func monthlyUsage(rows []AVAttendant, allowed map[string]bool, eligible map[int]bool) (map[int]int, map[int]map[string]bool) {
	counts := make(map[int]int)
	dates := make(map[int]map[string]bool)
	for _, row := range rows {
		if row.Assignee == nil || !allowed[row.Type] || !eligible[*row.Assignee] {
			continue
		}
		counts[*row.Assignee]++
		if dates[*row.Assignee] == nil {
			dates[*row.Assignee] = map[string]bool{}
		}
		dates[*row.Assignee][row.Date] = true
	}
	return counts, dates
}

func sortedTarget(target []AVAttendant) []AVAttendant {
	rows := append([]AVAttendant(nil), target...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Date != rows[j].Date {
			return rows[i].Date < rows[j].Date
		}
		if rows[i].Slot != rows[j].Slot {
			return rows[i].Slot < rows[j].Slot
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}

type cleanupPlanner struct {
	rules       CleanupRules
	allowed     map[string]bool
	names       map[int]string
	historical  map[int]int
	monthCounts map[int]int
	datesByUser map[int]map[string]bool
}

func (p *cleanupPlanner) applyRow(plan *CleanupPlan, row AVAttendant) {
	if reason := cleanupSkipReason(row, p.allowed); reason != "" {
		plan.Skipped = append(plan.Skipped, CleanupSkipped{SlotID: row.ID, Date: row.Date, Type: row.Type, Reason: reason})
		return
	}
	candidate, ok := chooseCleanupCandidate(p.rules, p.historical, p.monthCounts, p.datesByUser, row.Date)
	if !ok {
		plan.Skipped = append(plan.Skipped, CleanupSkipped{SlotID: row.ID, Date: row.Date, Type: row.Type, Reason: "no_eligible_candidate"})
		return
	}
	if row.UpdatedAt.IsZero() {
		plan.Skipped = append(plan.Skipped, CleanupSkipped{SlotID: row.ID, Date: row.Date, Type: row.Type, Reason: "missing_version"})
		return
	}
	plan.Mutations = append(plan.Mutations, CleanupMutation{
		SlotID: row.ID, Date: row.Date, Type: row.Type, Slot: row.Slot,
		AssigneeID: candidate, AssigneeName: p.names[candidate],
		UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339Nano),
	})
	p.monthCounts[candidate]++
	if p.datesByUser[candidate] == nil {
		p.datesByUser[candidate] = map[string]bool{}
	}
	p.datesByUser[candidate][row.Date] = true
}

func validateCleanupRules(rules CleanupRules) error {
	if len(rules.EligibleUserIDs) == 0 {
		return fmt.Errorf("eligible_user_ids is required")
	}
	if len(rules.Types) == 0 {
		return fmt.Errorf("types is required")
	}
	if rules.MaxAssignmentsPerUserPerMonth < 0 {
		return fmt.Errorf("max_assignments_per_user_per_month cannot be negative")
	}
	return nil
}

func cleanupSkipReason(row AVAttendant, allowedTypes map[string]bool) string {
	switch {
	case !allowedTypes[row.Type]:
		return "type_not_allowed"
	case row.Assignee != nil:
		return "already_assigned"
	case row.Extra:
		return "extra"
	case row.Published:
		return "published"
	case row.Confirmed:
		return "confirmed"
	case !isUnsentCleanupStatus(row.Status):
		return "status_not_unsent"
	}
	return ""
}

func isUnsentCleanupStatus(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	return status == "" || status == "naoenviado" || status == "não enviado" || status == "pending" || status == "esperando"
}

func chooseCleanupCandidate(rules CleanupRules, historical, monthly map[int]int, datesByUser map[int]map[string]bool, date string) (int, bool) {
	candidates := append([]int(nil), rules.EligibleUserIDs...)
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if historical[left] != historical[right] {
			return historical[left] < historical[right]
		}
		if monthly[left] != monthly[right] {
			return monthly[left] < monthly[right]
		}
		return left < right
	})
	for _, id := range candidates {
		if rules.MaxAssignmentsPerUserPerMonth > 0 && monthly[id] >= rules.MaxAssignmentsPerUserPerMonth {
			continue
		}
		if rules.DisallowSameDate && datesByUser[id][date] {
			continue
		}
		return id, true
	}
	return 0, false
}

// CleanupRulesDigest returns a stable digest without exposing policy contents in confirm tokens.
func CleanupRulesDigest(rules CleanupRules) string {
	users := append([]int(nil), rules.EligibleUserIDs...)
	types := append([]string(nil), rules.Types...)
	sort.Ints(users)
	sort.Strings(types)
	payload := fmt.Sprintf("%v|%v|%d|%t", users, types, rules.MaxAssignmentsPerUserPerMonth, rules.DisallowSameDate)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])[:32]
}

// CleanupPlanConfirmToken binds a preview to its canonical plan inputs and targets.
func CleanupPlanConfirmToken(plan CleanupPlan) string {
	var fields []string
	fields = append(fields, "cleanup-v1", plan.Month, plan.HistoryFrom, plan.HistoryTo, fmt.Sprint(plan.LGroup), plan.RulesDigest)
	for _, mutation := range plan.Mutations {
		fields = append(fields, fmt.Sprintf("%d|%s|%s|%d|%d|%s", mutation.SlotID, mutation.Date, mutation.Type, mutation.Slot, mutation.AssigneeID, mutation.UpdatedAt))
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "\n")))
	return hex.EncodeToString(sum[:])[:32]
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func intSet(values []int) map[int]bool {
	set := make(map[int]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func mapUsers(users []User) map[int]User {
	indexed := make(map[int]User, len(users))
	for _, user := range users {
		indexed[user.ID] = user
	}
	return indexed
}
