// Package cartimport turns a public witnessing spreadsheet into validated
// Hourglass slots. It never writes to the API; it produces a plan the caller
// can review and, once the write contract is confirmed, send.
package cartimport

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"hourglass-rejections-rpa/src/services/hourglass"
)

// Columns are the spreadsheet headers, in order.
var Columns = []string{"local", "data", "inicio", "fim", "publicador1", "publicador2"}

const dateLayoutBR = "02/01/2006"

// Row is one spreadsheet line with its original values.
type Row struct {
	Line       int
	Location   string
	Date       time.Time
	Start      string
	End        string
	Publisher1 string
	Publisher2 string
}

// Entry is one publisher assignment ready to be reviewed.
type Entry struct {
	Line          int
	LocationID    int
	LocationName  string
	Datetime      time.Time
	DurationHours float64
	PublisherID   int
	PublisherName string
}

// Problem blocks the whole import until it is fixed.
type Problem struct {
	Line    int
	Message string
}

func (p Problem) String() string {
	if p.Line == 0 {
		return p.Message
	}
	return fmt.Sprintf("linha %d: %s", p.Line, p.Message)
}

// Plan is the validated preview of an import.
type Plan struct {
	Entries  []Entry
	Problems []Problem
}

// OK reports whether the plan can be applied.
func (p Plan) OK() bool { return len(p.Problems) == 0 && len(p.Entries) > 0 }

// Parse reads the spreadsheet, validating structure and formats.
func Parse(source io.Reader) ([]Row, error) {
	reader := csv.NewReader(source)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("planilha inválida: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("planilha vazia")
	}
	if err := checkHeader(records[0]); err != nil {
		return nil, err
	}

	rows := make([]Row, 0, len(records)-1)
	problems := make([]string, 0)
	for index, record := range records[1:] {
		line := index + 2
		if blank(record) {
			continue
		}
		row, err := parseRow(line, record)
		if err != nil {
			problems = append(problems, fmt.Sprintf("linha %d: %v", line, err))
			continue
		}
		rows = append(rows, row)
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	if len(rows) == 0 {
		return nil, errors.New("planilha sem linhas de programação")
	}
	return rows, nil
}

func checkHeader(record []string) error {
	if len(record) < len(Columns) {
		return fmt.Errorf("cabeçalho deve ser: %s", strings.Join(Columns, ","))
	}
	for i, column := range Columns {
		got := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(record[i], "\ufeff")))
		if got != column {
			return fmt.Errorf("coluna %d deve ser %q, encontrado %q", i+1, column, record[i])
		}
	}
	return nil
}

func parseRow(line int, record []string) (Row, error) {
	for len(record) < len(Columns) {
		record = append(record, "")
	}
	values := make([]string, len(Columns))
	for i := range Columns {
		values[i] = strings.TrimSpace(record[i])
	}
	for i, name := range Columns[:4] {
		if values[i] == "" {
			return Row{}, fmt.Errorf("campo %q vazio", name)
		}
	}
	if values[4] == "" {
		return Row{}, errors.New("campo \"publicador1\" vazio")
	}

	date, err := parseDate(values[1])
	if err != nil {
		return Row{}, err
	}
	start, err := parseClock(values[2])
	if err != nil {
		return Row{}, fmt.Errorf("início: %w", err)
	}
	end, err := parseClock(values[3])
	if err != nil {
		return Row{}, fmt.Errorf("fim: %w", err)
	}
	if !end.After(start) {
		return Row{}, errors.New("fim deve ser depois do início")
	}

	return Row{
		Line:       line,
		Location:   values[0],
		Date:       date,
		Start:      start.Format("15:04"),
		End:        end.Format("15:04"),
		Publisher1: values[4],
		Publisher2: values[5],
	}, nil
}

func parseDate(value string) (time.Time, error) {
	for _, layout := range []string{dateLayoutBR, time.DateOnly} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("data %q inválida, use dd/mm/aaaa", value)
}

func parseClock(value string) (time.Time, error) {
	for _, layout := range []string{"15:04", "15h04", "15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("horário %q inválido, use hh:mm", value)
}

func blank(record []string) bool {
	for _, value := range record {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

// Build resolves names against Hourglass and reports every blocking problem.
func Build(rows []Row, locations []hourglass.PWLocation, users []hourglass.User, existing []hourglass.PWSlot) Plan {
	var plan Plan
	taken := make(map[string]hourglass.PWSlot, len(existing))
	for _, slot := range existing {
		taken[slotKey(slot.LocationID, slot.Datetime)] = slot
	}

	seen := make(map[string]int)
	busy := make(map[string]int)
	for _, row := range rows {
		location, problem := resolveLocation(row, locations)
		if problem != nil {
			plan.Problems = append(plan.Problems, *problem)
			continue
		}
		start, _ := time.Parse("15:04", row.Start)
		end, _ := time.Parse("15:04", row.End)
		datetime := time.Date(row.Date.Year(), row.Date.Month(), row.Date.Day(), start.Hour(), start.Minute(), 0, 0, time.UTC)

		for _, name := range []string{row.Publisher1, row.Publisher2} {
			if name == "" {
				continue
			}
			publisher, problem := resolvePublisher(row.Line, name, users)
			if problem != nil {
				plan.Problems = append(plan.Problems, *problem)
				continue
			}
			slotID := fmt.Sprintf("%d|%s|%s|%d", location.ID, row.Date.Format(time.DateOnly), row.Start, publisher.ID)
			if first, duplicate := seen[slotID]; duplicate {
				plan.Problems = append(plan.Problems, Problem{row.Line, fmt.Sprintf(
					"repetido: %s já tem %s em %s %s (linha %d)",
					location.Name, publisherName(publisher), row.Date.Format(dateLayoutBR), row.Start, first)})
				continue
			}
			seen[slotID] = row.Line
			if _, exists := taken[slotKey(location.ID, datetime.Format(time.RFC3339))]; exists {
				plan.Problems = append(plan.Problems, Problem{row.Line, fmt.Sprintf(
					"já existe programação em %s para %s %s",
					location.Name, row.Date.Format(dateLayoutBR), row.Start)})
			}
			publisherSlot := fmt.Sprintf("%d|%s|%s", publisher.ID, row.Date.Format(time.DateOnly), row.Start)
			if first, clash := busy[publisherSlot]; clash {
				plan.Problems = append(plan.Problems, Problem{row.Line, fmt.Sprintf(
					"%s já está escalado no mesmo horário (linha %d)", publisherName(publisher), first)})
				continue
			}
			busy[publisherSlot] = row.Line

			plan.Entries = append(plan.Entries, Entry{
				Line:          row.Line,
				LocationID:    location.ID,
				LocationName:  location.Name,
				Datetime:      datetime,
				DurationHours: end.Sub(start).Hours(),
				PublisherID:   publisher.ID,
				PublisherName: publisherName(publisher),
			})
		}
	}
	sort.SliceStable(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].Datetime.Equal(plan.Entries[j].Datetime) {
			return plan.Entries[i].LocationName < plan.Entries[j].LocationName
		}
		return plan.Entries[i].Datetime.Before(plan.Entries[j].Datetime)
	})
	return plan
}

func slotKey(locationID int, datetime string) string {
	return fmt.Sprintf("%d|%s", locationID, datetime)
}

func resolveLocation(row Row, locations []hourglass.PWLocation) (hourglass.PWLocation, *Problem) {
	var matches []hourglass.PWLocation
	for _, location := range locations {
		if strings.EqualFold(strings.TrimSpace(location.Name), row.Location) {
			matches = append(matches, location)
		}
	}
	switch len(matches) {
	case 0:
		return hourglass.PWLocation{}, &Problem{row.Line, fmt.Sprintf("local %q não encontrado no Hourglass", row.Location)}
	case 1:
		if matches[0].Disabled {
			return hourglass.PWLocation{}, &Problem{row.Line, fmt.Sprintf("local %q está desativado", row.Location)}
		}
		return matches[0], nil
	default:
		return hourglass.PWLocation{}, &Problem{row.Line, fmt.Sprintf("local %q é ambíguo: %d cadastros", row.Location, len(matches))}
	}
}

func resolvePublisher(line int, name string, users []hourglass.User) (hourglass.User, *Problem) {
	var matches []hourglass.User
	for _, user := range users {
		if strings.EqualFold(publisherName(user), name) || strings.EqualFold(fullName(user), name) {
			matches = append(matches, user)
		}
	}
	switch len(matches) {
	case 0:
		return hourglass.User{}, &Problem{line, fmt.Sprintf("publicador %q não encontrado", name)}
	case 1:
		return matches[0], nil
	default:
		return hourglass.User{}, &Problem{line, fmt.Sprintf("publicador %q é ambíguo: %d cadastros", name, len(matches))}
	}
}

func publisherName(user hourglass.User) string {
	if name := strings.TrimSpace(hourglass.UserDisplayName(user)); name != "" {
		return name
	}
	return fullName(user)
}

func fullName(user hourglass.User) string {
	return fmt.Sprintf("%s %s", strings.TrimSpace(user.Firstname), strings.TrimSpace(user.Lastname))
}
