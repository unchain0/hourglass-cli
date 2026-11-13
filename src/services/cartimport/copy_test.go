package cartimport

import (
	"strings"
	"testing"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

func id(value int) *int { return &value }

func december() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }

func dates(plan Plan) []string {
	out := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		out = append(out, entry.Datetime.Format(time.DateOnly))
	}
	return out
}

func TestCopyMonthRepeatsEveryMatchingWeekday(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	plan := CopyMonth(source, december(), nil)

	if !plan.OK() {
		t.Fatalf("unexpected problems: %v", plan.Problems)
	}
	want := []string{"2026-12-03", "2026-12-10", "2026-12-17", "2026-12-24", "2026-12-31"}
	if got := dates(plan); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected dates: %v", got)
	}
	if plan.Entries[0].DurationHours != 2 || plan.Entries[0].PublisherID != 100 {
		t.Fatalf("unexpected entry: %+v", plan.Entries[0])
	}
}

func TestCopyMonthKeepsUnassignedSlotsAndDuration(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 2, LocationID: 12, Datetime: "2026-11-06T09:30:00Z", Duration: "1.50", AssigneeID: nil},
	}
	plan := CopyMonth(source, december(), nil)

	if !plan.OK() || len(plan.Entries) != 4 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, entry := range plan.Entries {
		if entry.PublisherID != 0 || entry.DurationHours != 1.5 {
			t.Fatalf("unexpected entry: %+v", entry)
		}
		if entry.Datetime.Weekday() != time.Friday {
			t.Fatalf("unexpected weekday: %s", entry.Datetime.Weekday())
		}
	}
}

func TestCopyMonthSkipsSlotsAlreadyPresent(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	existing := []hourglass.PWSlot{
		{ID: 9, LocationID: 10, Datetime: "2026-12-10T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	plan := CopyMonth(source, december(), existing)

	if !plan.OK() || len(plan.Entries) != 4 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, date := range dates(plan) {
		if date == "2026-12-10" {
			t.Fatal("existing slot must not be created again")
		}
	}
}

func TestCopyMonthCollapsesDuplicateSourceRows(t *testing.T) {
	row := hourglass.PWSlot{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: nil}
	plan := CopyMonth([]hourglass.PWSlot{row, row}, december(), nil)

	if !plan.OK() || len(plan.Entries) != 5 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestCopyMonthReportsDoubleBookedPublishers(t *testing.T) {
	source := []hourglass.PWSlot{
		{ID: 1, LocationID: 10, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
		{ID: 2, LocationID: 11, Datetime: "2026-11-05T08:00:00Z", Duration: "2.00", AssigneeID: id(100)},
	}
	plan := CopyMonth(source, december(), nil)

	if plan.OK() {
		t.Fatal("expected problems")
	}
	if !strings.Contains(joinProblems(plan), "dois locais") {
		t.Fatalf("unexpected problems: %v", joinProblems(plan))
	}
}

func joinProblems(plan Plan) string {
	out := make([]string, 0, len(plan.Problems))
	for _, problem := range plan.Problems {
		out = append(out, problem.String())
	}
	return strings.Join(out, "\n")
}
