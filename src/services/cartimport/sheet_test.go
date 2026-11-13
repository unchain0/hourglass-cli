package cartimport

import (
	"testing"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

func sheetFixture() ([]hourglass.PWLocation, []hourglass.User) {
	return []hourglass.PWLocation{{ID: 1, Name: "Local Exemplo 1"}},
		[]hourglass.User{
			{ID: 10, Descriptor: "Pessoa Exemplo A"},
			{ID: 11, Descriptor: "Pessoa Exemplo B"},
		}
}

func TestToSheetGroupsPublishersAndPreservesTime(t *testing.T) {
	locations, users := sheetFixture()
	slots := []hourglass.PWSlot{
		{ID: 1, LocationID: 1, Datetime: "2026-12-03T08:00:00Z", Duration: "2.00", AssigneeID: id(11)},
		{ID: 2, LocationID: 1, Datetime: "2026-12-03T08:00:00Z", Duration: "2.00", AssigneeID: id(10)},
	}
	rows, problems := ToSheet(slots, locations, users)

	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one grouped row, got %+v", rows)
	}
	row := rows[0]
	if row.Location != "Local Exemplo 1" || row.Date != "03/12/2026" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.Start != "08:00" || row.End != "10:00" {
		t.Fatalf("times should match slot datetime directly: %+v", row)
	}
	if row.Publisher1 != "Pessoa Exemplo A" || row.Publisher2 != "Pessoa Exemplo B" {
		t.Fatalf("unexpected publishers: %+v", row)
	}
}

func TestDesiredPreservesSlotTimes(t *testing.T) {
	locations, users := sheetFixture()
	rows := []Row{{Line: 2, Location: "Local Exemplo 1", Date: mustDate("03/12/2026"), Start: "08:00", End: "10:00", Publisher1: "Pessoa Exemplo A"}}

	entries, problems := Desired(rows, locations, users)
	if len(problems) != 0 || len(entries) != 1 {
		t.Fatalf("unexpected result: %+v %v", entries, problems)
	}
	if got := entries[0].Datetime.Format(time.RFC3339); got != "2026-12-03T08:00:00Z" {
		t.Fatalf("unexpected instant: %s", got)
	}
	if entries[0].DurationHours != 2 {
		t.Fatalf("unexpected duration: %+v", entries[0])
	}
}

func TestCompareFindsCreatesAndDeletes(t *testing.T) {
	locations, users := sheetFixture()
	rows := []Row{{Line: 2, Location: "Local Exemplo 1", Date: mustDate("03/12/2026"), Start: "08:00", End: "10:00", Publisher1: "Pessoa Exemplo A"}}
	entries, _ := Desired(rows, locations, users)

	live := []hourglass.PWSlot{
		{ID: 5, LocationID: 1, Datetime: "2026-12-03T08:00:00Z", Duration: "2.00", AssigneeID: id(10)},
		{ID: 6, LocationID: 1, Datetime: "2026-12-04T08:00:00Z", Duration: "2.00", AssigneeID: id(10)},
	}
	diff := Compare(entries, live)

	if len(diff.Create) != 0 {
		t.Fatalf("slot in the sheet must not be created again: %+v", diff.Create)
	}
	if len(diff.Delete) != 1 || diff.Delete[0].ID != 6 {
		t.Fatalf("slot missing from the sheet must be deleted: %+v", diff.Delete)
	}

	diff = Compare(entries, live[:1])
	if !diff.IsEmpty() {
		t.Fatalf("expected empty diff, got %+v", diff)
	}

	diff = Compare(nil, live[:1])
	if len(diff.Delete) != 1 || len(diff.Create) != 0 {
		t.Fatalf("unexpected diff: %+v", diff)
	}
}

func mustDate(value string) time.Time {
	parsed, err := time.Parse(dateLayoutBR, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
