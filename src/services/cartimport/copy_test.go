package cartimport

import (
	"strings"
	"testing"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

func id(value int) *int { return &value }

func TestCopyMonthKeepsWeekdayOrdinalAndTime(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
		{ID: 2, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(101)},
		{ID: 3, LocationID: 11, Datetime: "2026-11-05T10:00:00Z", Duration: "3.00", AssigneeID: id(102)},
	}
	plan := CopyMonth(source, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), nil)

	if !plan.OK() {
		t.Fatalf("unexpected problems: %v", plan.Problems)
	}
	if len(plan.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(plan.Entries))
	}
	// 2026-11-05 is the first Thursday of November; December's first Thursday is the 3rd.
	for _, entry := range plan.Entries[:2] {
		if got := entry.Datetime.Format(time.RFC3339); got != "2026-12-03T08:00:00Z" {
			t.Fatalf("unexpected datetime: %s", got)
		}
	}
	if got := plan.Entries[2].Datetime.Format(time.RFC3339); got != "2026-12-03T10:00:00Z" {
		t.Fatalf("unexpected datetime: %s", got)
	}
	if plan.Entries[0].DurationHours != 2 || plan.Entries[2].DurationHours != 3 {
		t.Fatalf("unexpected durations: %+v", plan.Entries)
	}
}

func TestCopyMonthRejectsMissingFifthWeek(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-30T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	plan := CopyMonth(source, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), nil)

	if plan.OK() {
		t.Fatal("expected problem for a fifth week that the target month lacks")
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("unexpected entries: %+v", plan.Entries)
	}
}

func TestCopyMonthSkipsSlotsAlreadyPresent(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
		{ID: 2, LocationID: 11, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	existing := []hourglass.PWSlot{
		{ID: 9, LocationID: 10, Datetime: "2026-12-03T08:00:00Z", Duration: "2.00", AssigneeID: id(200)},
	}
	plan := CopyMonth(source, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), existing)

	if plan.OK() {
		t.Fatal("expected problems")
	}
	text := ""
	for _, problem := range plan.Problems {
		text += problem.String() + "\n"
	}
	for _, want := range []string{"dois locais"} {
		if !strings.Contains(text, want) {
			t.Fatalf("problems %q should mention %q", text, want)
		}
	}
}

func TestCopyMonthSkipsDuplicatesAndKeepsUnassignedSlots(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
		{ID: 2, LocationID: 12, Datetime: "2026-11-06T09:30:00Z", Duration: "1.50", AssigneeID: nil},
	}
	plan := CopyMonth(source, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), nil)

	if !plan.OK() || len(plan.Entries) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if plan.Entries[1].PublisherID != 0 || plan.Entries[1].DurationHours != 1.5 {
		t.Fatalf("unexpected unassigned entry: %+v", plan.Entries[1])
	}
	if got := plan.Entries[1].Datetime.Format(time.RFC3339); got != "2026-12-04T09:30:00Z" {
		t.Fatalf("unexpected datetime: %s", got)
	}
}
