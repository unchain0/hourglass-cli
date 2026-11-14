package hourglass

import (
	"fmt"
	"sort"
)

// IndexUsersByID builds a lookup map from user ID to User.
func IndexUsersByID(users []User) map[int]User {
	index := make(map[int]User, len(users))
	for _, user := range users {
		index[user.ID] = user
	}
	return index
}

// UserDisplayName returns the preferred display name for a user.
func UserDisplayName(user User) string {
	if user.Descriptor != "" {
		return user.Descriptor
	}
	name := fmt.Sprintf("%s %s", user.Firstname, user.Lastname)
	if name == " " {
		return fmt.Sprintf("User %d", user.ID)
	}
	return name
}

// LookupUserName resolves a user ID through the index.
func LookupUserName(index map[int]User, userID int) string {
	if user, ok := index[userID]; ok {
		return UserDisplayName(user)
	}
	return fmt.Sprintf("User %d", userID)
}

// LookupOptionalUserName resolves a nullable user ID.
func LookupOptionalUserName(index map[int]User, userID *int) string {
	if userID == nil {
		return ""
	}
	return LookupUserName(index, *userID)
}

// PartTitlesFromMeetings maps midweek part IDs to titles.
func PartTitlesFromMeetings(meetings []Meeting) map[int]string {
	titles := make(map[int]string)
	for _, meeting := range meetings {
		for _, part := range meeting.TGW {
			titles[part.ID] = part.Title
		}
		for _, part := range meeting.FM {
			titles[part.ID] = part.Title
		}
		for _, part := range meeting.LAC {
			titles[part.ID] = part.Title
		}
	}
	return titles
}

func assignmentTitle(notif Notification, partTitles map[int]string) string {
	if title := partTitles[notif.Part]; title != "" {
		return title
	}
	if notif.Type == "mm" {
		return getMidweekFlagName(notif.Flag)
	}
	return getFriendlyTypeName(notif.Type)
}

// HasSubstitute reports whether another pending/complete notification covers the
// same slot after a decline (a substitute assignment).
func HasSubstitute(all []Notification, declined Notification) bool {
	for _, candidate := range all {
		if candidate.ID == declined.ID {
			continue
		}
		if candidate.Date != declined.Date ||
			candidate.Part != declined.Part ||
			candidate.Type != declined.Type ||
			candidate.Classroom != declined.Classroom {
			continue
		}
		if candidate.Assignee == declined.Assignee {
			continue
		}
		switch NotificationStatus(candidate.Status) {
		case StatusPending, StatusComplete:
			return true
		}
	}
	return false
}

// EnrichDeclinedNotifications keeps declined rows and adds name/assignment/substitute fields.
func EnrichDeclinedNotifications(
	notifications []Notification,
	users map[int]User,
	partTitles map[int]string,
) []DeclinedAssignment {
	if partTitles == nil {
		partTitles = map[int]string{}
	}
	out := make([]DeclinedAssignment, 0, len(notifications))
	for _, notif := range notifications {
		if NotificationStatus(notif.Status) != StatusDeclined {
			continue
		}
		out = append(out, DeclinedAssignment{
			ID:                 notif.ID,
			Type:               notif.Type,
			Date:               notif.Date,
			Part:               notif.Part,
			Classroom:          notif.Classroom,
			Assignment:         assignmentTitle(notif, partTitles),
			AssigneeID:         notif.Assignee,
			AssigneeName:       LookupUserName(users, notif.Assignee),
			Status:             notif.Status,
			SubstituteAssigned: HasSubstitute(notifications, notif),
		})
	}
	return out
}

// EnrichAVAttendants resolves assignee names while preserving AV status/type fields.
func EnrichAVAttendants(attendants []AVAttendant, users map[int]User) []AVAttendantView {
	out := make([]AVAttendantView, 0, len(attendants))
	for _, row := range attendants {
		out = append(out, AVAttendantView{
			ID:           row.ID,
			Type:         row.Type,
			AssigneeID:   row.Assignee,
			AssigneeName: LookupOptionalUserName(users, row.Assignee),
			Slot:         row.Slot,
			Date:         row.Date,
			Extra:        row.Extra,
			Status:       row.Status,
			Confirmed:    row.Confirmed,
			Published:    row.Published,
			CreatedAt:    row.CreatedAt,
			UpdatedAt:    row.UpdatedAt,
		})
	}
	return out
}

// meetingPartIDs collects TGW/FM/LAC part IDs that belong to one meeting.
func meetingPartIDs(meeting Meeting) map[int]struct{} {
	ids := make(map[int]struct{}, len(meeting.TGW)+len(meeting.FM)+len(meeting.LAC))
	for _, part := range meeting.TGW {
		ids[part.ID] = struct{}{}
	}
	for _, part := range meeting.FM {
		ids[part.ID] = struct{}{}
	}
	for _, part := range meeting.LAC {
		ids[part.ID] = struct{}{}
	}
	return ids
}

// assignmentsForMeeting keeps only rows whose part ID appears on the meeting.
// soleOnDate lets a part-less meeting keep the full date feed when it is the only
// meeting for that date; with siblings present, part-less meetings get nothing so
// statuses are not duplicated across groups.
func assignmentsForMeeting(rows []MMAssignmentView, parts map[int]struct{}, soleOnDate bool) []MMAssignmentView {
	if len(parts) == 0 {
		if !soleOnDate || len(rows) == 0 {
			return nil
		}
		out := make([]MMAssignmentView, len(rows))
		copy(out, rows)
		return out
	}
	out := make([]MMAssignmentView, 0, len(rows))
	for _, row := range rows {
		if _, ok := parts[row.PartID]; ok {
			out = append(out, row)
		}
	}
	return out
}

// EnrichMeetingsWithAssignments attaches per-assignment status rows from mm notifications.
// Rows are scoped to each meeting's part IDs so multiple meetings on one date do not
// inherit each other's assignment statuses.
func EnrichMeetingsWithAssignments(
	meetings []Meeting,
	notifications []Notification,
	users map[int]User,
) []MeetingView {
	byDate := assignmentsByDate(notifications, PartTitlesFromMeetings(meetings), users)
	perDate := meetingsPerDate(meetings)

	state := enrichState{
		perDate: perDate,
		claimed: make(map[string]map[int]struct{}, len(meetings)),
		tookAll: make(map[string]bool, len(meetings)),
		seen:    make(map[string]bool, len(meetings)),
	}
	out := make([]MeetingView, 0, len(meetings))
	for _, meeting := range meetings {
		out = append(out, state.meetingView(meeting, byDate))
	}
	return state.withLeftovers(out, byDate)
}

type enrichState struct {
	perDate map[string]int
	claimed map[string]map[int]struct{}
	tookAll map[string]bool
	seen    map[string]bool
}

func assignmentsByDate(notifications []Notification, titles map[int]string, users map[int]User) map[string][]MMAssignmentView {
	byDate := make(map[string][]MMAssignmentView)
	for _, notif := range notifications {
		if notif.Type != "" && notif.Type != "mm" {
			// Keep non-mm rows out of the midweek view when a mixed feed is passed.
			continue
		}
		byDate[notif.Date] = append(byDate[notif.Date], MMAssignmentView{
			Date:         notif.Date,
			PartID:       notif.Part,
			Title:        assignmentTitle(notif, titles),
			Flag:         notif.Flag,
			AssigneeID:   notif.Assignee,
			AssigneeName: LookupUserName(users, notif.Assignee),
			Status:       notif.Status,
			Classroom:    notif.Classroom,
		})
	}
	return byDate
}

func meetingsPerDate(meetings []Meeting) map[string]int {
	perDate := make(map[string]int, len(meetings))
	for _, meeting := range meetings {
		perDate[meeting.Date]++
	}
	return perDate
}

func (s *enrichState) meetingView(meeting Meeting, byDate map[string][]MMAssignmentView) MeetingView {
	s.seen[meeting.Date] = true
	parts := meetingPartIDs(meeting)
	soleOnDate := s.perDate[meeting.Date] == 1
	assignments := assignmentsForMeeting(byDate[meeting.Date], parts, soleOnDate)
	s.claim(meeting.Date, parts, soleOnDate, len(assignments))
	return MeetingView{
		Date:        meeting.Date,
		LGroup:      meeting.LGroup,
		TGW:         meeting.TGW,
		FM:          meeting.FM,
		LAC:         meeting.LAC,
		Assignments: assignments,
	}
}

func (s *enrichState) claim(date string, parts map[int]struct{}, soleOnDate bool, assignmentCount int) {
	if len(parts) == 0 {
		if soleOnDate && assignmentCount > 0 {
			s.tookAll[date] = true
		}
		return
	}
	claimed := s.claimed[date]
	if claimed == nil {
		claimed = make(map[int]struct{}, len(parts))
		s.claimed[date] = claimed
	}
	for partID := range parts {
		claimed[partID] = struct{}{}
	}
}

// withLeftovers adds notification-only dates and rows whose parts matched no meeting.
func (s *enrichState) withLeftovers(out []MeetingView, byDate map[string][]MMAssignmentView) []MeetingView {
	for date, assignments := range byDate {
		if !s.seen[date] {
			out = append(out, MeetingView{Date: date, Assignments: assignments})
			continue
		}
		if s.tookAll[date] {
			continue
		}
		leftovers := unclaimed(assignments, s.claimed[date])
		if len(leftovers) == 0 {
			continue
		}
		out = append(out, MeetingView{Date: date, Assignments: leftovers})
	}
	return out
}

func unclaimed(assignments []MMAssignmentView, claimed map[int]struct{}) []MMAssignmentView {
	var leftovers []MMAssignmentView
	for _, row := range assignments {
		if _, ok := claimed[row.PartID]; ok {
			continue
		}
		leftovers = append(leftovers, row)
	}
	return leftovers
}

// EnrichWMAssignments resolves assignee names for WM rows.
func EnrichWMAssignments(rows []WMAssignment, users map[int]User) []WMAssignmentView {
	out := make([]WMAssignmentView, 0, len(rows))
	for _, row := range rows {
		out = append(out, WMAssignmentView{
			ID:           row.ID,
			Role:         row.Role,
			AssigneeID:   row.Assignee,
			AssigneeName: LookupOptionalUserName(users, row.Assignee),
			Status:       row.Status,
			Date:         row.Date,
			Extra:        row.Extra,
			Confirmed:    row.Confirmed,
			Published:    row.Published,
			CreatedAt:    row.CreatedAt,
			UpdatedAt:    row.UpdatedAt,
		})
	}
	return out
}

// EnrichPrivileges resolves assignee names for privilege rows.
func EnrichPrivileges(rows []PrivilegeAssignment, users map[int]User) []PrivilegeAssignmentView {
	out := make([]PrivilegeAssignmentView, 0, len(rows))
	for _, row := range rows {
		out = append(out, PrivilegeAssignmentView{
			ID:           row.ID,
			Type:         row.Type,
			AssigneeID:   row.Assignee,
			AssigneeName: LookupOptionalUserName(users, row.Assignee),
			Status:       row.Status,
			Date:         row.Date,
			Confirmed:    row.Confirmed,
			Published:    row.Published,
		})
	}
	return out
}

// BuildWMSchedule combines the AV attendant slots and the weekend part
// notifications into one read-only schedule. Weekend part rows carry the
// part id the API exposes; it does not label speaker, chairman or reader.
func BuildWMSchedule(
	attendants []AVAttendant,
	parts []WeekendNotification,
	users map[int]User,
) []WMScheduleEntry {
	entries := make([]WMScheduleEntry, 0, len(attendants)+len(parts))
	for _, row := range attendants {
		entries = append(entries, WMScheduleEntry{
			Role:         row.Type,
			Date:         row.Date,
			AssigneeID:   row.Assignee,
			AssigneeName: LookupOptionalUserName(users, row.Assignee),
			Status:       row.Status,
			Source:       "av_attendant",
		})
	}
	for _, row := range parts {
		entries = append(entries, WMScheduleEntry{
			Role:         "weekend_part",
			PartID:       row.Part,
			Date:         row.Date,
			AssigneeID:   row.Assignee,
			AssigneeName: LookupOptionalUserName(users, row.Assignee),
			Status:       row.Status,
			Source:       "notifications/fm",
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Date != entries[j].Date {
			return entries[i].Date < entries[j].Date
		}
		return entries[i].Role < entries[j].Role
	})
	return entries
}
