package cartimport

import (
	"fmt"
	"sort"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

// ExportRow is one cart slot as a person reads it: one location and one hour,
// with up to two publishers.
type ExportRow struct {
	Location   string
	Date       string
	Start      string
	End        string
	Publisher1 string
	Publisher2 string
}

// ToSheet builds spreadsheet rows from live slots, grouping the publishers that
// share a location and hour. The hours match the slot datetime directly.
func ToSheet(slots []hourglass.PWSlot, locations []hourglass.PWLocation, users []hourglass.User) ([]ExportRow, []Problem) {
	names := userNames(users)
	places := locationNames(locations)
	grouped, problems := groupSlots(slots, names)
	rows := sheetRows(grouped, places)
	sortSheet(rows)
	return rows, problems
}

type slotGroup struct {
	location int
	at       time.Time
	end      time.Time
	people   []string
}

func userNames(users []hourglass.User) map[int]string {
	names := make(map[int]string, len(users))
	for _, user := range users {
		names[user.ID] = publisherName(user)
	}
	return names
}

func locationNames(locations []hourglass.PWLocation) map[int]string {
	places := make(map[int]string, len(locations))
	for _, location := range locations {
		places[location.ID] = location.Name
	}
	return places
}

func groupSlots(slots []hourglass.PWSlot, names map[int]string) (map[string]*slotGroup, []Problem) {
	grouped := make(map[string]*slotGroup, len(slots))
	var problems []Problem
	for _, slot := range slots {
		at, err := time.Parse(time.RFC3339, slot.Datetime)
		if err != nil {
			problems = append(problems, Problem{0, fmt.Sprintf("horário inválido: %q", slot.Datetime)})
			continue
		}
		key := fmt.Sprintf("%d|%s|%s", slot.LocationID, at.Format(time.RFC3339), slot.Duration)
		entry := grouped[key]
		if entry == nil {
			entry = &slotGroup{
				location: slot.LocationID,
				at:       at,
				end:      at.Add(time.Duration(durationHours(slot.Duration) * float64(time.Hour))),
			}
			grouped[key] = entry
		}
		if slot.AssigneeID != nil {
			entry.people = append(entry.people, names[*slot.AssigneeID])
		}
	}
	return grouped, problems
}

func sheetRows(grouped map[string]*slotGroup, places map[int]string) []ExportRow {
	rows := make([]ExportRow, 0, len(grouped))
	for _, entry := range grouped {
		sort.Strings(entry.people)
		row := ExportRow{
			Location: places[entry.location],
			Date:     entry.at.Format(dateLayoutBR),
			Start:    entry.at.Format("15:04"),
			End:      entry.end.Format("15:04"),
		}
		if len(entry.people) > 0 {
			row.Publisher1 = entry.people[0]
		}
		if len(entry.people) > 1 {
			row.Publisher2 = entry.people[1]
		}
		rows = append(rows, row)
	}
	return rows
}

func sortSheet(rows []ExportRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Date == rows[j].Date {
			return rows[i].Start < rows[j].Start
		}
		first, errA := time.Parse(dateLayoutBR, rows[i].Date)
		second, errB := time.Parse(dateLayoutBR, rows[j].Date)
		if errA != nil || errB != nil {
			return rows[i].Date < rows[j].Date
		}
		return first.Before(second)
	})
}

// Desired turns spreadsheet rows into the slots the sheet asks for. It never
// looks at the live schedule; callers compare the result with it.
func Desired(rows []Row, locations []hourglass.PWLocation, users []hourglass.User) ([]Entry, []Problem) {
	var entries []Entry
	var problems []Problem
	for _, row := range rows {
		rowEntries, rowProblems := desiredRow(row, locations, users)
		entries = append(entries, rowEntries...)
		problems = append(problems, rowProblems...)
	}
	return entries, problems
}

func desiredRow(row Row, locations []hourglass.PWLocation, users []hourglass.User) ([]Entry, []Problem) {
	location, problem := resolveLocation(row, locations)
	if problem != nil {
		return nil, []Problem{*problem}
	}
	start, err := parseClock(row.Start)
	if err != nil {
		return nil, []Problem{{row.Line, fmt.Sprintf("início: %v", err)}}
	}
	end, err := parseClock(row.End)
	if err != nil {
		return nil, []Problem{{row.Line, fmt.Sprintf("fim: %v", err)}}
	}
	datetime := time.Date(row.Date.Year(), row.Date.Month(), row.Date.Day(), start.Hour(), start.Minute(), 0, 0, time.UTC)
	duration := end.Sub(start).Hours()

	var entries []Entry
	var problems []Problem
	for _, name := range []string{row.Publisher1, row.Publisher2} {
		if name == "" {
			continue
		}
		publisher, problem := resolvePublisher(row.Line, name, users)
		if problem != nil {
			problems = append(problems, *problem)
			continue
		}
		entries = append(entries, Entry{
			Line:          row.Line,
			LocationID:    location.ID,
			LocationName:  location.Name,
			Datetime:      datetime,
			DurationHours: duration,
			PublisherID:   publisher.ID,
			PublisherName: publisherName(publisher),
		})
	}
	if len(entries) == 0 && len(problems) == 0 {
		entries = append(entries, Entry{
			Line:          row.Line,
			LocationID:    location.ID,
			LocationName:  location.Name,
			Datetime:      datetime,
			DurationHours: duration,
		})
	}
	return entries, problems
}

// MonthDiff describes what the live schedule must lose and gain to match the sheet.
type MonthDiff struct {
	Create []Entry
	Delete []hourglass.PWSlot
}

// IsEmpty reports whether the sheet already matches the live schedule.
func (d MonthDiff) IsEmpty() bool { return len(d.Create) == 0 && len(d.Delete) == 0 }

// Compare matches desired entries with live slots by location, instant and
// publisher.
func Compare(desired []Entry, live []hourglass.PWSlot) MonthDiff {
	want := make(map[string]Entry, len(desired))
	for _, entry := range desired {
		want[slotIdentity(entry.LocationID, entry.Datetime, entry.DurationHours, entry.PublisherID)] = entry
	}
	var diff MonthDiff
	for _, slot := range live {
		at, err := time.Parse(time.RFC3339, slot.Datetime)
		if err != nil {
			continue
		}
		dur := durationHours(slot.Duration)
		key := slotIdentity(slot.LocationID, at.UTC(), dur, assignee(slot))
		if _, keep := want[key]; keep {
			delete(want, key)
			continue
		}
		diff.Delete = append(diff.Delete, slot)
	}
	for _, entry := range want {
		diff.Create = append(diff.Create, entry)
	}
	sort.Slice(diff.Create, func(i, j int) bool {
		return diff.Create[i].Datetime.Before(diff.Create[j].Datetime)
	})
	return diff
}

func slotIdentity(locationID int, at time.Time, duration float64, publisher int) string {
	return fmt.Sprintf("%d|%s|%.2f|%d", locationID, at.UTC().Format(time.RFC3339), duration, publisher)
}
