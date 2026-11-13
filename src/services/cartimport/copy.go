package cartimport

import (
	"fmt"
	"sort"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

type slotTemplate struct {
	hour      int
	minute    int
	location  int
	duration  string
	publisher int
}

// CopyMonth repeats one representative week of the source month across the whole
// target month. For each weekday it uses the most recent source date, which is
// the roster currently in force, and applies it to every matching weekday of the
// target month.
func CopyMonth(source []hourglass.PWSlot, target time.Time, existing []hourglass.PWSlot) Plan {
	plan := Plan{Entries: []Entry{}}
	week := latestWeek(source, &plan)
	// A slot is identified by location, instant and publisher, so two publishers
	// sharing a location hour stay independent and a re-run stays idempotent.
	taken := make(map[string]struct{}, len(existing))
	for _, slot := range existing {
		taken[slotKey(slot.LocationID, normalize(slot.Datetime))+fmt.Sprintf("|%d", assignee(slot))] = struct{}{}
	}

	first := time.Date(target.Year(), target.Month(), 1, 0, 0, 0, 0, time.UTC)
	days := daysInMonth(first)
	state := emitState{
		seen:  make(map[string]struct{}, len(source)*2),
		busy:  make(map[string]struct{}, len(source)*2),
		taken: taken,
	}
	for day := 0; day < days; day++ {
		date := first.AddDate(0, 0, day)
		for _, template := range week[date.Weekday()] {
			state.emit(&plan, date, template)
		}
	}

	sort.SliceStable(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].Datetime.Equal(plan.Entries[j].Datetime) {
			return plan.Entries[i].LocationID < plan.Entries[j].LocationID
		}
		return plan.Entries[i].Datetime.Before(plan.Entries[j].Datetime)
	})
	return plan
}

// latestWeek keeps only the slots of the most recent date of each weekday.
func latestWeek(source []hourglass.PWSlot, plan *Plan) map[time.Weekday][]slotTemplate {
	latest := make(map[time.Weekday]time.Time, 7)
	week := make(map[time.Weekday][]slotTemplate, 7)
	for _, slot := range source {
		at, err := time.Parse(time.RFC3339, slot.Datetime)
		if err != nil {
			plan.Problems = append(plan.Problems, Problem{0, fmt.Sprintf("horário inválido na origem: %q", slot.Datetime)})
			continue
		}
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		weekday := at.Weekday()
		if day.After(latest[weekday]) {
			latest[weekday] = day
			week[weekday] = nil
		}
		if !day.Equal(latest[weekday]) {
			continue
		}
		week[weekday] = append(week[weekday], slotTemplate{
			hour:      at.Hour(),
			minute:    at.Minute(),
			location:  slot.LocationID,
			duration:  slot.Duration,
			publisher: assignee(slot),
		})
	}
	return week
}

type emitState struct {
	seen  map[string]struct{}
	busy  map[string]struct{}
	taken map[string]struct{}
}

func (s emitState) emit(plan *Plan, date time.Time, template slotTemplate) {
	datetime := time.Date(date.Year(), date.Month(), date.Day(), template.hour, template.minute, 0, 0, time.UTC)
	when := datetime.Format(time.RFC3339)
	key := slotKey(template.location, when) + fmt.Sprintf("|%d", template.publisher)
	if _, duplicate := s.seen[key]; duplicate {
		return
	}
	s.seen[key] = struct{}{}
	if template.publisher != 0 {
		publisherKey := fmt.Sprintf("%d|%s", template.publisher, when)
		if _, clash := s.busy[publisherKey]; clash {
			plan.Problems = append(plan.Problems, Problem{0, fmt.Sprintf(
				"publicador %d ficaria em dois locais em %s", template.publisher, datetime.Format("02/01/2006 15:04"))})
			return
		}
		s.busy[publisherKey] = struct{}{}
	}
	if _, exists := s.taken[key]; exists {
		return
	}
	plan.Entries = append(plan.Entries, Entry{
		LocationID:    template.location,
		Datetime:      datetime,
		DurationHours: durationHours(template.duration),
		PublisherID:   template.publisher,
	})
}

func daysInMonth(first time.Time) int {
	return time.Date(first.Year(), first.Month()+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1).Day()
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
