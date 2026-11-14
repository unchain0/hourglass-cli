package cartimport

import (
	"strings"
	"testing"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

func TestParseReadsBrazilianDatesAndOptionalSecondPublisher(t *testing.T) {
	source := "local,data,inicio,fim,publicador1,publicador2\r\n" +
		"Local Exemplo 1,02/12/2026,08:00,10:00,Pessoa Exemplo A,\r\n"

	rows, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.Location != "Local Exemplo 1" || row.Start != "08:00" || row.End != "10:00" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.Publisher1 != "Pessoa Exemplo A" || row.Publisher2 != "" {
		t.Fatalf("unexpected publishers: %+v", row)
	}
	if got := row.Date.Format(time.DateOnly); got != "2026-12-02" {
		t.Fatalf("unexpected date: %s", got)
	}
}

func TestParseAcceptsBOMAndIsoDates(t *testing.T) {
	source := "\ufeffLocal,Data,Inicio,Fim,Publicador1,Publicador2\n" +
		"Local Exemplo 2,2026-12-05,09:00,11:00,Pessoa Exemplo A,Pessoa Exemplo B\n"

	rows, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 1 || rows[0].Start != "09:00" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestParseReportsInvalidLinesTogether(t *testing.T) {
	source := "local,data,inicio,fim,publicador1,publicador2\n" +
		"Local Exemplo 2,31/02/2026,09:00,11:00,Pessoa Exemplo A,\n" +
		"Local Exemplo 2,02/12/2026,9h,11:00,Pessoa Exemplo A,\n" +
		"Local Exemplo 2,02/12/2026,11:00,09:00,Pessoa Exemplo A,\n"

	_, err := Parse(strings.NewReader(source))
	if err == nil {
		t.Fatal("expected parse error")
	}
	for _, want := range []string{"linha 2", "linha 3", "linha 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %s", err, want)
		}
	}
}

func TestParseRejectsWrongHeader(t *testing.T) {
	_, err := Parse(strings.NewReader("local,dia,inicio\nLocal Exemplo 2,02/12/2026,09:00\n"))
	if err == nil || !strings.Contains(err.Error(), "cabeçalho") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func fixture() ([]hourglass.PWLocation, []hourglass.User) {
	locations := []hourglass.PWLocation{
		{ID: 1, Name: "Local Exemplo 1"},
		{ID: 2, Name: "Local Exemplo 2"},
		{ID: 3, Name: "Fechado", Disabled: true},
	}
	users := []hourglass.User{
		{ID: 10, Firstname: "Pessoa", Lastname: "Exemplo A", Descriptor: "Pessoa Exemplo A"},
		{ID: 11, Firstname: "Pessoa", Lastname: "Exemplo B", Descriptor: "Pessoa Exemplo B"},
		{ID: 12, Firstname: "Pessoa", Lastname: "Exemplo C", Descriptor: "Pessoa Exemplo C"},
	}
	return locations, users
}

func row(line int, location, date, start, end, first, second string) Row {
	parsed, _ := time.Parse(dateLayoutBR, date)
	return Row{Line: line, Location: location, Date: parsed, Start: start, End: end, Publisher1: first, Publisher2: second}
}

func TestBuildResolvesPublishersAndComputesDuration(t *testing.T) {
	locations, users := fixture()
	plan := Build([]Row{row(2, "Local Exemplo 2", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo B", "Pessoa Exemplo C")}, locations, users, nil)

	if !plan.OK() {
		t.Fatalf("unexpected problems: %v", plan.Problems)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("expected two publishers, got %d", len(plan.Entries))
	}
	for _, entry := range plan.Entries {
		if entry.LocationID != 2 || entry.DurationHours != 2 {
			t.Fatalf("unexpected entry: %+v", entry)
		}
		if got := entry.Datetime.Format(time.RFC3339); got != "2026-12-02T08:00:00Z" {
			t.Fatalf("unexpected datetime: %s", got)
		}
	}
}

func TestBuildRejectsUnknownDisabledAndAmbiguousNames(t *testing.T) {
	locations, users := fixture()
	users = append(users, hourglass.User{ID: 13, Firstname: "Pessoa", Lastname: "Exemplo D", Descriptor: "Pessoa Exemplo D"})
	plan := Build([]Row{
		row(2, "Inexistente", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo B", ""),
		row(3, "Fechado", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo B", ""),
		row(4, "Local Exemplo 2", "02/12/2026", "08:00", "10:00", "Pessoa Inexistente", ""),
	}, locations, users, nil)

	if plan.OK() {
		t.Fatal("expected problems")
	}
	if len(plan.Problems) != 3 {
		t.Fatalf("expected 3 problems, got %v", plan.Problems)
	}
}

func TestBuildBlocksDuplicatesExistingSlotsAndDoubleBooking(t *testing.T) {
	locations, users := fixture()
	existing := []hourglass.PWSlot{{ID: 99, LocationID: 2, Datetime: "2026-12-02T08:00:00Z", Duration: "2.00"}}
	plan := Build([]Row{
		row(2, "Local Exemplo 2", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo B", ""),
		row(3, "Local Exemplo 2", "03/12/2026", "08:00", "10:00", "Pessoa Exemplo B", "Pessoa Exemplo B"),
		row(4, "Local Exemplo 1", "03/12/2026", "08:00", "10:00", "Pessoa Exemplo B", ""),
	}, locations, users, existing)

	messages := make([]string, 0, len(plan.Problems))
	for _, problem := range plan.Problems {
		messages = append(messages, problem.String())
	}
	joined := strings.Join(messages, "\n")
	for _, want := range []string{"já existe", "repetido", "mesmo horário"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("problems %q should mention %q", joined, want)
		}
	}
	if plan.OK() {
		t.Fatal("plan with problems must not be applicable")
	}
}

func TestParseSkipsBlankLinesAndAcceptsClockVariants(t *testing.T) {
	source := "local,data,inicio,fim,publicador1,publicador2\n" +
		",,,,,\n" +
		"Local Exemplo 2,2026-12-05,09h00,11h00,Pessoa Exemplo B,\n"

	rows, err := Parse(strings.NewReader(source))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rows) != 1 || rows[0].Start != "09:00" || rows[0].End != "11:00" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestParseRejectsShortHeaderAndEmptySpreadsheet(t *testing.T) {
	if _, err := Parse(strings.NewReader("local,data\n")); err == nil || !strings.Contains(err.Error(), "cabeçalho") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Parse(strings.NewReader("")); err == nil {
		t.Fatal("expected error for empty spreadsheet")
	}
	if _, err := Parse(strings.NewReader(strings.Join(Columns, ",") + "\n")); err == nil || !strings.Contains(err.Error(), "sem linhas") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProblemStringWithoutLineOmitsPrefix(t *testing.T) {
	if got := (Problem{Message: "sem hora"}).String(); got != "sem hora" {
		t.Fatalf("unexpected message: %s", got)
	}
	if got := (Problem{Line: 7, Message: "x"}).String(); got != "linha 7: x" {
		t.Fatalf("unexpected message: %s", got)
	}
}

func TestBuildUsesFullNameWhenDescriptorIsEmpty(t *testing.T) {
	locations, users := fixture()
	users = append(users, hourglass.User{ID: 14, Firstname: "Pessoa", Lastname: "Exemplo E"})
	plan := Build([]Row{row(2, "Local Exemplo 2", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo E", "")}, locations, users, nil)

	if !plan.OK() || len(plan.Entries) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if plan.Entries[0].PublisherID != 14 || plan.Entries[0].PublisherName != "Pessoa Exemplo E" {
		t.Fatalf("unexpected entry: %+v", plan.Entries[0])
	}
}

func TestBuildReportsAmbiguousLocations(t *testing.T) {
	locations := []hourglass.PWLocation{{ID: 1, Name: "Local Exemplo 2"}, {ID: 2, Name: "local exemplo 2"}}
	_, users := fixture()
	plan := Build([]Row{row(2, "LOCAL EXEMPLO 2", "02/12/2026", "08:00", "10:00", "Pessoa Exemplo B", "")}, locations, users, nil)

	if plan.OK() || !strings.Contains(strings.Join(problemTexts(plan), "\n"), "ambíguo") {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func problemTexts(plan Plan) []string {
	out := make([]string, 0, len(plan.Problems))
	for _, problem := range plan.Problems {
		out = append(out, problem.String())
	}
	return out
}

func TestParseReportsMalformedCSV(t *testing.T) {
	source := "local,data,inicio,fim,publicador1,publicador2\n\"Local Exemplo 2,02/12/2026,08:00,10:00,Pessoa Exemplo B,\n"
	if _, err := Parse(strings.NewReader(source)); err == nil || !strings.Contains(err.Error(), "planilha inválida") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildMatchesDescriptorCaseInsensitively(t *testing.T) {
	locations, users := fixture()
	plan := Build([]Row{row(2, "Local Exemplo 2", "05/12/2026", "08:00", "10:00", "pessoa exemplo b", "PESSOA EXEMPLO C")}, locations, users, nil)

	if !plan.OK() || len(plan.Entries) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}
