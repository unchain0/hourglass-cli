package cartimport

import (
	"fmt"
	"sort"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

// CopyMonth maps every slot of a source month into the target month, keeping the
// weekday and the time of day. A slot on the 2nd Thursday becomes the 2nd
// Thursday of the target month, and so on.
func CopyMonth(source []hourglass.PWSlot, target time.Time, existing []hourglass.PWSlot) Plan {
	var plan Plan
	// A slot is identified by location, instant and publisher, so two publishers
	// sharing a location hour stay independent and a re-run stays idempotent.
	taken := make(map[string]struct{}, len(existing))
	for _, slot := range existing {
		taken[slotKey(slot.LocationID, normalize(slot.Datetime))+fmt.Sprintf("|%d", assignee(slot))] = struct{}{}
	}
	seen := make(map[string]struct{}, len(source))
	busy := make(map[string]struct{}, len(source))

	for _, slot := range source {
		at, err := time.Parse(time.RFC3339, slot.Datetime)
		if err != nil {
			plan.Problems = append(plan.Problems, Problem{0, fmt.Sprintf("horário inválido na origem: %q", slot.Datetime)})
			continue
		}
		day, ok := sameWeekdayOrdinal(at, target)
		if !ok {
			plan.Problems = append(plan.Problems, Problem{0, fmt.Sprintf(
				"%s não tem a mesma semana de %s para replicar %s",
				target.Format("01/2006"), at.Format("02/01/2006"), at.Format("02/01/2006"))})
			continue
		}
		datetime := time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
		when := datetime.Format(time.RFC3339)
		key := slotKey(slot.LocationID, when) + fmt.Sprintf("|%d", assignee(slot))
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if slot.AssigneeID != nil {
			publisherKey := fmt.Sprintf("%d|%s", *slot.AssigneeID, when)
			if _, clash := busy[publisherKey]; clash {
				plan.Problems = append(plan.Problems, Problem{0, fmt.Sprintf(
					"publicador %d ficaria em dois locais em %s", *slot.AssigneeID, datetime.Format("02/01/2006 15:04"))})
				continue
			}
			busy[publisherKey] = struct{}{}
		}
		if _, exists := taken[key]; exists {
			continue
		}
		entry := Entry{
			LocationID:    slot.LocationID,
			Datetime:      datetime,
			DurationHours: durationHours(slot.Duration),
			PublisherID:   assignee(slot),
		}
		plan.Entries = append(plan.Entries, entry)
	}
	sort.SliceStable(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].Datetime.Equal(plan.Entries[j].Datetime) {
			return plan.Entries[i].LocationID < plan.Entries[j].LocationID
		}
		return plan.Entries[i].Datetime.Before(plan.Entries[j].Datetime)
	})
	return plan
}

func assignee(slot hourglass.PWSlot) int {
	if slot.AssigneeID == nil {
		return 0
	}
	return *slot.AssigneeID
}

func durationHours(raw string) float64 {
	var hours float64
	if _, err := fmt.Sscanf(raw, "%f", &hours); err != nil {
		return 0
	}
	return hours
}

func normalize(datetime string) string {
	at, err := time.Parse(time.RFC3339, datetime)
	if err != nil {
		return datetime
	}
	return at.UTC().Format(time.RFC3339)
}

// sameWeekdayOrdinal returns the date in target with the same weekday and the
// same occurrence number (1st..5th) as the source date.
func sameWeekdayOrdinal(source, target time.Time) (time.Time, bool) {
	ordinal := (source.Day()-1)/7 + 1
	first := time.Date(target.Year(), target.Month(), 1, 0, 0, 0, 0, time.UTC)
	offset := (int(source.Weekday()) - int(first.Weekday()) + 7) % 7
	day := first.AddDate(0, 0, offset+7*(ordinal-1))
	if day.Month() != target.Month() {
		return time.Time{}, false
	}
	return day, true
}
