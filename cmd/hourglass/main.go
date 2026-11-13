package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"hourglass-rejections-rpa/src/integrations/auth/webauthn"
	"hourglass-rejections-rpa/src/services/cartimport"
	"hourglass-rejections-rpa/src/services/hourglass"

	_ "github.com/glebarez/go-sqlite"
)

var (
	osExit      = os.Exit
	marshalJSON = json.Marshal
)

const (
	cmdAssign   = "assign"
	flagJSON    = "--json"
	flagDryRun  = "--dry-run"
	flagConfirm = "--confirm"
	flagSlot    = "--slot"
	flagTo      = "--to"
	flagFrom    = "--from"
	flagUntil   = "--until"
	flagLGroup  = "--lgroup"
)

// CLI is the read-only Hourglass command-line surface.
type CLI struct {
	client       *hourglass.Client
	stdout       io.Writer
	stderr       io.Writer
	getenv       func(string) string
	ensureTokens func() (*webauthn.AuthTokens, error)
}

// NewCLI creates a CLI whose every HTTP request passes through the send blocklist.
func NewCLI() *CLI {
	return newCLI(os.Stdout, os.Stderr, os.Getenv)
}

func newCLI(stdout, stderr io.Writer, getenv func(string) string) *CLI {
	if getenv == nil {
		getenv = os.Getenv
	}
	client := hourglass.NewClient()
	allowSend := getenv("HOURGLASS_ALLOW_SEND") == "1"
	allowAssign := getenv("HOURGLASS_ALLOW_ASSIGN") == "1"
	client.SetTransport(&blocklistTransport{
		base:        client.GetTransport(),
		allowSend:   allowSend,
		allowAssign: allowAssign,
		allowCart:   getenv("HOURGLASS_ALLOW_CART") == "1",
	})
	return &CLI{
		client: client,
		stdout: stdout,
		stderr: stderr,
		getenv: getenv,
	}
}

// bootstrapWebAuthn wires credentials/tokens/profile env into the Hourglass client.
// Enables the token manager and loads persisted tokens. Does not start the
// background renewal loop; one-shot CLI calls use EnsureValidTokens on demand.
func (c *CLI) bootstrapWebAuthn() func() {
	credentialsPath := c.getenv("WEBAUTHN_CREDENTIALS_PATH")
	tokensPath := c.getenv("WEBAUTHN_TOKENS_PATH")
	if tokensPath == "" {
		tokensPath = c.getenv("TOKENS_PATH")
	}
	profileDir := c.getenv("CHROME_PROFILE_DIR")
	baseURL := c.getenv("HOURGLASS_URL")

	if baseURL != "" {
		c.client.SetBaseURL(baseURL)
	}
	if tokensPath != "" {
		c.client.SetWebAuthnTokensPath(tokensPath)
	}
	if profileDir != "" {
		c.client.SetBrowserProfileDir(profileDir)
	}

	if credentialsPath == "" && profileDir == "" && tokensPath == "" {
		return nil
	}

	if err := c.client.EnableWebAuthn(credentialsPath, nil); err != nil {
		return nil
	}
	_ = c.client.LoadPersistedAuthTokens()
	return func() { c.client.StopTokenManager() }
}

// EnsureValidTokens reports current WebAuthn session tokens when configured.
func (c *CLI) EnsureValidTokens() (*webauthn.AuthTokens, error) {
	if c.ensureTokens != nil {
		return c.ensureTokens()
	}
	return c.client.EnsureValidTokens()
}

type exitCode2Error struct {
	msg string
}

func (e *exitCode2Error) Error() string {
	return e.msg
}

func exitCode2(msg string) error {
	return &exitCode2Error{msg: msg}
}

// AuthStatus reports credential/token presence without printing secret values.
func (c *CLI) AuthStatus() error {
	credentialsPath := c.getenv("WEBAUTHN_CREDENTIALS_PATH")
	tokensPath := c.getenv("WEBAUTHN_TOKENS_PATH")

	status := map[string]any{
		"webauthn_credentials_path": credentialsPath,
		"tokens_path":               tokensPath,
		"has_credentials":           credentialsPath != "",
		"has_tokens":                tokensPath != "",
	}

	if tokensPath == "" {
		status["tokens_valid"] = false
		status["tokens_error"] = "no tokens path configured"
	} else {
		tokens, err := c.EnsureValidTokens()
		if err != nil {
			status["tokens_valid"] = false
			status["tokens_error"] = err.Error()
		} else {
			status["tokens_valid"] = true
			status["tokens_expires_at"] = tokens.ExpiresAt.Format(time.RFC3339)
			status["hglogin_present"] = tokens.HGLogin != ""
			status["xsrf_present"] = tokens.XSRFToken != ""
		}
	}

	out, err := marshalJSON(status)
	if err != nil {
		return fmt.Errorf("failed to marshal auth status: %w", err)
	}
	fmt.Fprintln(c.stdout, string(out))
	return nil
}

// AuthLogin runs headless passkey authentication when credentials exist.
// It never registers a passkey; missing credentials exit 2 with setup-auth guidance.
func (c *CLI) AuthLogin() error {
	credentialsPath := c.getenv("WEBAUTHN_CREDENTIALS_PATH")
	if credentialsPath == "" {
		return exitCode2("passkey not registered: run setup-auth")
	}
	if _, err := os.Stat(credentialsPath); os.IsNotExist(err) {
		return exitCode2("passkey not registered: run setup-auth")
	}
	tokens, err := c.EnsureValidTokens()
	if err != nil {
		return fmt.Errorf("passkey login failed: %w", err)
	}
	if tokens == nil {
		return errors.New("passkey login failed: no tokens returned")
	}
	fmt.Fprintf(c.stdout, "passkey login: ok, tokens valid until %s\n", tokens.ExpiresAt.Format(time.RFC3339))
	return nil
}

// Users prints GET /fsreport/users as JSON.
func (c *CLI) Users() error {
	users, err := c.client.GetUsers()
	if err != nil {
		return fmt.Errorf("failed to get users: %w", err)
	}
	return c.printJSON(users)
}

// ScheduleMeetings prints midweek meetings with per-assignment acceptance/decline status.
func (c *CLI) ScheduleMeetings(start, end string, lgroup int) error {
	meetings, err := c.client.GetMeetings(start, end, lgroup)
	if err != nil {
		return fmt.Errorf("failed to get meetings: %w", err)
	}
	notifications, err := c.client.GetNotifications(start, end, "mm")
	if err != nil {
		return fmt.Errorf("failed to get mm assignment statuses: %w", err)
	}
	users, err := c.loadUserIndex()
	if err != nil {
		return err
	}
	return c.printJSON(hourglass.EnrichMeetingsWithAssignments(meetings, notifications, users))
}

// ScheduleAVAttendants prints AV rows with type, status, and resolved assignee names.
func (c *CLI) ScheduleAVAttendants(start, end string) error {
	attendants, err := c.client.GetAVAttendants(start, end)
	if err != nil {
		return fmt.Errorf("failed to get AV attendants: %w", err)
	}
	users, err := c.loadUserIndex()
	if err != nil {
		return err
	}
	return c.printJSON(hourglass.EnrichAVAttendants(attendants, users))
}

// ScheduleNotifications prints declined assignments enriched with name, assignment, and substitute status.
// When notifType is empty, every DefaultNotificationTypes section is fetched.
func (c *CLI) ScheduleNotifications(start, end, notifType string) error {
	var (
		notifications []hourglass.Notification
		err           error
	)
	if notifType == "" {
		notifications, err = c.client.GetNotificationsByTypes(start, end, hourglass.DefaultNotificationTypes)
	} else {
		notifications, err = c.client.GetNotifications(start, end, notifType)
	}
	if err != nil {
		return fmt.Errorf("failed to get notifications: %w", err)
	}

	users, err := c.loadUserIndex()
	if err != nil {
		return err
	}

	partTitles := map[int]string{}
	if needsMMTitles(notifType, notifications) {
		if meetings, meetErr := c.client.GetMeetings(start, end, 0); meetErr == nil {
			partTitles = hourglass.PartTitlesFromMeetings(meetings)
		}
	}

	return c.printJSON(hourglass.EnrichDeclinedNotifications(notifications, users, partTitles))
}

func needsMMTitles(notifType string, notifications []hourglass.Notification) bool {
	if notifType == "" || notifType == "mm" {
		return true
	}
	for _, n := range notifications {
		if n.Type == "mm" {
			return true
		}
	}
	return false
}

func (c *CLI) loadUserIndex() (map[int]hourglass.User, error) {
	users, err := c.client.GetUsers()
	if err != nil {
		return nil, fmt.Errorf("failed to get users: %w", err)
	}
	return hourglass.IndexUsersByID(users), nil
}

func (c *CLI) printJSON(v any) error {
	out, err := marshalJSON(v)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}
	fmt.Fprintln(c.stdout, string(out))
	return nil
}

// ScheduleWM prints Watchtower Meeting assignments with status and resolved names.
func (c *CLI) ScheduleWM(start, end string) error {
	// scheduling/wm returns 404 on the current API, so build the read-only
	// schedule from the endpoints the web app still uses.
	attendants, err := c.client.GetAVAttendants(start, end)
	if err != nil {
		return fmt.Errorf("failed to get AV attendants: %w", err)
	}
	parts, err := c.client.GetWeekendNotifications(start, end)
	if err != nil {
		return fmt.Errorf("failed to get weekend part notifications: %w", err)
	}
	users, err := c.loadUserIndex()
	if err != nil {
		return err
	}
	return c.printJSON(hourglass.BuildWMSchedule(attendants, parts, users))
}

// Privileges prints privilege rows with status and resolved assignee names.
func (c *CLI) Privileges() error {
	privileges, err := c.client.GetPrivileges()
	if err != nil {
		return fmt.Errorf("failed to get privileges: %w", err)
	}
	users, err := c.loadUserIndex()
	if err != nil {
		return err
	}
	return c.printJSON(hourglass.EnrichPrivileges(privileges, users))
}

// Assign is not implemented yet; dry-run unless --apply is supplied by the caller.
func (c *CLI) Assign() error {
	return exitCode2("not implemented yet: endpoint not discovered")
}

// Unassign is not implemented yet; dry-run unless --apply is supplied by the caller.
func (c *CLI) Unassign() error {
	return exitCode2("not implemented yet: endpoint not discovered")
}

// AssignSwap changes only the assignee of one AV slot.
// Dry-run prints before/after and a confirm token. Apply requires HOURGLASS_ALLOW_ASSIGN=1
// and --confirm matching that token. Never publishes, notifies, or sends.
func (c *CLI) AssignSwap(opts assignSwapOpts) error {
	if err := validateAssignSwapOpts(opts, c.getenv); err != nil {
		return err
	}
	from, until := resolveAssignSwapRange(opts)
	row, err := c.client.FindAVAttendantByID(opts.slotID, from, until, opts.lgroup)
	if err != nil {
		return err
	}
	usersList, err := c.client.GetUsers()
	if err != nil {
		return fmt.Errorf("failed to get users: %w", err)
	}
	target, err := hourglass.ResolveUserRef(opts.toUser, usersList)
	if err != nil {
		return err
	}
	users := hourglass.IndexUsersByID(usersList)
	preview := hourglass.BuildAssignSwapPreview(*row, target, opts.lgroup, users)
	if opts.dryRun {
		return c.printJSON(preview)
	}
	return c.applyAssignSwap(opts, from, until, target, users, preview)
}

func validateAssignSwapOpts(opts assignSwapOpts, getenv func(string) string) error {
	if opts.slotID == 0 {
		return errors.New("missing required --slot")
	}
	if strings.TrimSpace(opts.toUser) == "" {
		return errors.New("missing required --to")
	}
	if opts.dryRun {
		return nil
	}
	if getenv("HOURGLASS_ALLOW_ASSIGN") != "1" {
		return exitCode2("assign swap disabled: set HOURGLASS_ALLOW_ASSIGN=1 and pass --confirm from a matching dry-run")
	}
	if strings.TrimSpace(opts.confirm) == "" {
		return exitCode2("missing --confirm token; run with --dry-run first")
	}
	return nil
}

func resolveAssignSwapRange(opts assignSwapOpts) (from, until string) {
	from, until = opts.fromDate, opts.untilDate
	defFrom, defUntil := hourglass.DefaultAssignSwapRange(time.Now())
	if from == "" {
		from = defFrom
	}
	if until == "" {
		until = defUntil
	}
	return from, until
}

func (c *CLI) applyAssignSwap(
	opts assignSwapOpts,
	from, until string,
	target hourglass.User,
	users map[int]hourglass.User,
	preview hourglass.AssignSwapResult,
) error {
	// Re-load immediately before write so a concurrent edit invalidates confirm.
	fresh, err := c.client.FindAVAttendantByID(opts.slotID, from, until, opts.lgroup)
	if err != nil {
		return err
	}
	live := hourglass.BuildAssignSwapPreview(*fresh, target, opts.lgroup, users)
	if subtle.ConstantTimeCompare([]byte(opts.confirm), []byte(live.Confirm)) != 1 {
		return exitCode2("confirm token mismatch or stale preview; re-run --dry-run")
	}
	next := *fresh
	tid := target.ID
	next.Assignee = &tid
	updated, err := c.client.SaveAVAttendantAssignee(next, opts.lgroup)
	if err != nil {
		return fmt.Errorf("failed to swap assignee: %w", err)
	}
	result := preview
	result.DryRun = false
	result.Applied = true
	result.Confirm = ""
	result.Before = live.Before
	result.After.AssigneeID = updated.Assignee
	result.After.AssigneeName = hourglass.LookupOptionalUserName(users, updated.Assignee)
	if !updated.UpdatedAt.IsZero() {
		result.After.UpdatedAt = hourglass.AssignSwapVersion(*updated)
	}
	return c.printJSON(result)
}

// CheckRules retrieves check rules status information as JSON.
func (c *CLI) CheckRules() error {
	rules, err := c.client.GetCheckRules()
	if err != nil {
		return fmt.Errorf("failed to get check rules: %w", err)
	}
	return c.printJSON(rules)
}

func (c *CLI) dispatch(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(c.stderr, "Usage: hourglass <command> [args...]\n")
		return errors.New("no command provided")
	}

	switch args[0] {
	case "auth":
		return c.runAuth(args[1:])
	case "users":
		return c.runUsers(args[1:])
	case "schedule":
		return c.runSchedule(args[1:])
	case cmdAssign:
		return c.runAssign(args[1:])
	case "cleanup":
		return c.runCleanup(args[1:])
	default:
		fmt.Fprintf(c.stderr, "Unknown command: %s\n", args[0])
		return errors.New("unknown command")
	}
}

type cleanupOpts struct {
	fromDate, toDate string
	month, rulesPath string
	types            []string
	historyMonths    int
	lgroup           int
	apply            bool
	confirm          string
}

func (c *CLI) runCleanup(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: hourglass cleanup <history|plan> [flags]")
	}
	opts, err := parseCleanupArgs(args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "history":
		if opts.fromDate == "" || opts.toDate == "" || len(opts.types) == 0 {
			return errors.New("usage: hourglass cleanup history --from YYYY-MM-DD --to YYYY-MM-DD --types type[,type] --json")
		}
		return c.CleanupHistory(opts.fromDate, opts.toDate, opts.types)
	case "plan":
		return c.CleanupPlan(opts)
	default:
		return fmt.Errorf("unknown cleanup subcommand %s", args[0])
	}
}

func parseCleanupArgs(args []string) (cleanupOpts, error) {
	opts := cleanupOpts{historyMonths: 3}
	for i := 0; i < len(args); {
		var value string
		var next int
		var err error
		switch args[i] {
		case flagFrom:
			value, next, err = takeFlagValue(args, i, flagFrom)
			opts.fromDate = value
		case flagTo:
			value, next, err = takeFlagValue(args, i, flagTo)
			opts.toDate = value
		case "--month":
			value, next, err = takeFlagValue(args, i, "--month")
			opts.month = value
		case "--rules":
			value, next, err = takeFlagValue(args, i, "--rules")
			opts.rulesPath = value
		case "--types":
			value, next, err = takeFlagValue(args, i, "--types")
			opts.types = splitTypes(value)
		case "--history-months":
			value, next, err = takeFlagValue(args, i, "--history-months")
			if err == nil {
				opts.historyMonths, err = strconv.Atoi(value)
			}
		case flagLGroup:
			value, next, err = takeFlagValue(args, i, flagLGroup)
			if err == nil {
				opts.lgroup, err = strconv.Atoi(value)
			}
		case "--apply":
			opts.apply = true
			next = i + 1
		case flagConfirm:
			value, next, err = takeFlagValue(args, i, flagConfirm)
			opts.confirm = value
		case flagJSON:
			next = i + 1
		default:
			return opts, fmt.Errorf("unknown flag %s", args[i])
		}
		if err != nil {
			return opts, err
		}
		i = next
	}
	return opts, nil
}

func splitTypes(raw string) []string {
	var types []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			types = append(types, value)
		}
	}
	return types
}

func (c *CLI) CleanupHistory(from, to string, types []string) error {
	rows, err := c.client.GetAVAttendants(from, to)
	if err != nil {
		return fmt.Errorf("failed to get AV history: %w", err)
	}
	allowed := make(map[string]bool, len(types))
	for _, typ := range types {
		allowed[typ] = true
	}
	filtered := rows[:0]
	for _, row := range rows {
		if allowed[row.Type] {
			filtered = append(filtered, row)
		}
	}
	return c.printJSON(filtered)
}

func (c *CLI) CleanupPlan(opts cleanupOpts) error {
	if opts.month == "" || opts.rulesPath == "" || len(opts.types) == 0 {
		return errors.New("usage: hourglass cleanup plan --month YYYY-MM --rules FILE --types type[,type] [--history-months N] [--lgroup N] [--apply --confirm TOKEN] --json")
	}
	month, err := time.Parse("2006-01", opts.month)
	if err != nil {
		return fmt.Errorf("invalid --month %q", opts.month)
	}
	if opts.historyMonths < 1 {
		return errors.New("--history-months must be at least 1")
	}
	rules, err := loadCleanupRules(opts.rulesPath)
	if err != nil {
		return err
	}
	rules.Types = opts.types
	plan, err := c.buildCleanupPlan(month, rules, opts.historyMonths, opts.lgroup)
	if err != nil {
		return err
	}
	if !opts.apply {
		return c.printJSON(plan)
	}
	if c.getenv("HOURGLASS_ALLOW_SEND") == "1" {
		return exitCode2("cleanup apply refused while HOURGLASS_ALLOW_SEND=1")
	}
	if c.getenv("HOURGLASS_ALLOW_ASSIGN") != "1" {
		return exitCode2("cleanup apply disabled: set HOURGLASS_ALLOW_ASSIGN=1")
	}
	if opts.confirm == "" {
		return exitCode2("missing --confirm token; run without --apply first")
	}
	live, err := c.buildCleanupPlan(month, rules, opts.historyMonths, opts.lgroup)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(opts.confirm), []byte(live.Confirm)) != 1 {
		return exitCode2("confirm token mismatch or stale preview; re-run without --apply")
	}
	for _, mutation := range live.Mutations {
		row := hourglass.AVAttendant{ID: mutation.SlotID, Date: mutation.Date, Type: mutation.Type, Slot: mutation.Slot, Assignee: &mutation.AssigneeID}
		if _, err := c.client.SaveAVAttendantAssignee(row, opts.lgroup); err != nil {
			return fmt.Errorf("failed to apply cleanup assignment: %w", err)
		}
	}
	live.DryRun = false
	live.Confirm = ""
	return c.printJSON(live)
}

func (c *CLI) buildCleanupPlan(month time.Time, rules hourglass.CleanupRules, historyMonths, lgroup int) (hourglass.CleanupPlan, error) {
	historyFrom := month.AddDate(0, -historyMonths, 0).Format("2006-01-02")
	historyTo := month.AddDate(0, 0, -1).Format("2006-01-02")
	targetFrom := month.Format("2006-01-02")
	targetTo := month.AddDate(0, 1, -1).Format("2006-01-02")
	history, err := c.client.GetAVAttendants(historyFrom, historyTo)
	if err != nil {
		return hourglass.CleanupPlan{}, fmt.Errorf("failed to get cleanup history: %w", err)
	}
	target, err := c.client.GetAVAttendants(targetFrom, targetTo)
	if err != nil {
		return hourglass.CleanupPlan{}, fmt.Errorf("failed to get cleanup target: %w", err)
	}
	users, err := c.client.GetUsers()
	if err != nil {
		return hourglass.CleanupPlan{}, fmt.Errorf("failed to get users: %w", err)
	}
	return hourglass.BuildCleanupPlan(month, history, target, users, rules, historyMonths, lgroup)
}

func loadCleanupRules(path string) (hourglass.CleanupRules, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return hourglass.CleanupRules{}, fmt.Errorf("failed to read cleanup rules: %w", err)
	}
	var rules hourglass.CleanupRules
	if err := json.Unmarshal(data, &rules); err != nil {
		return hourglass.CleanupRules{}, fmt.Errorf("failed to parse cleanup rules: %w", err)
	}
	return rules, nil
}

type assignSwapOpts struct {
	slotID    int
	toUser    string
	confirm   string
	fromDate  string
	untilDate string
	lgroup    int
	dryRun    bool
}

func (c *CLI) runAssign(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(c.stderr, "Usage: hourglass assign swap --slot <id> --to <user> [--from YYYY-MM-DD] [--until YYYY-MM-DD] [--lgroup N] (--dry-run | --confirm <token>)\n")
		return errors.New("no assign subcommand provided")
	}
	switch args[0] {
	case "swap":
		opts, err := parseAssignSwapArgs(args[1:])
		if err != nil {
			return err
		}
		return c.AssignSwap(opts)
	default:
		fmt.Fprintf(c.stderr, "Unknown assign subcommand: %s\n", args[0])
		fmt.Fprintf(c.stderr, "Usage: hourglass assign swap --slot <id> --to <user> [--dry-run|--confirm <token>]\n")
		return errors.New("unknown assign subcommand")
	}
}

func parseAssignSwapArgs(args []string) (assignSwapOpts, error) {
	var opts assignSwapOpts
	for i := 0; i < len(args); {
		next, err := applyAssignSwapFlag(&opts, args, i)
		if err != nil {
			return opts, err
		}
		i = next
	}
	if opts.dryRun && opts.confirm != "" {
		return opts, errors.New("use either --dry-run or --confirm, not both")
	}
	if !opts.dryRun && opts.confirm == "" {
		// Default to dry-run when neither flag is set so apply stays opt-in.
		opts.dryRun = true
	}
	return opts, nil
}

func applyAssignSwapFlag(opts *assignSwapOpts, args []string, i int) (int, error) {
	switch args[i] {
	case flagSlot:
		return setAssignSwapInt(args, i, flagSlot, func(n int) error {
			if n <= 0 {
				return fmt.Errorf("invalid %s value %q", flagSlot, args[i+1])
			}
			opts.slotID = n
			return nil
		})
	case flagTo:
		return setAssignSwapString(args, i, flagTo, &opts.toUser)
	case flagConfirm:
		return setAssignSwapString(args, i, flagConfirm, &opts.confirm)
	case flagFrom:
		return setAssignSwapString(args, i, flagFrom, &opts.fromDate)
	case flagUntil:
		return setAssignSwapString(args, i, flagUntil, &opts.untilDate)
	case flagLGroup:
		return setAssignSwapInt(args, i, flagLGroup, func(n int) error {
			opts.lgroup = n
			return nil
		})
	case flagDryRun:
		opts.dryRun = true
		return i + 1, nil
	case flagJSON:
		return i + 1, nil
	default:
		return i, fmt.Errorf("unknown flag %s", args[i])
	}
}

func setAssignSwapString(args []string, i int, name string, dst *string) (int, error) {
	val, next, err := takeFlagValue(args, i, name)
	if err != nil {
		return i, err
	}
	*dst = val
	return next, nil
}

func setAssignSwapInt(args []string, i int, name string, apply func(int) error) (int, error) {
	val, next, err := takeFlagValue(args, i, name)
	if err != nil {
		return i, err
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return i, fmt.Errorf("invalid %s value %q", name, val)
	}
	if err := apply(n); err != nil {
		return i, err
	}
	return next, nil
}

func (c *CLI) BootstrapFirefox(profile, tokensOut string) error {
	if profile == "" || tokensOut == "" {
		return errors.New("usage: hourglass auth bootstrap-firefox --profile FIREFOX_PROFILE --tokens-out FILE")
	}
	cookiesPath := filepath.Join(filepath.Clean(profile), "cookies.sqlite")
	db, err := sql.Open("sqlite", "file:"+cookiesPath+"?mode=ro&immutable=1")
	if err != nil {
		return fmt.Errorf("failed to open Firefox cookies database: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name, value, expiry FROM moz_cookies WHERE host IN (?, ?) AND name IN (?, ?)`, ".app.hourglass-app.com", "app.hourglass-app.com", "hglogin", "X-Hourglass-XSRF-Token")
	if err != nil {
		return fmt.Errorf("failed to read Firefox cookies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tokens := webauthn.AuthTokens{}
	var earliestExpiry time.Time
	for rows.Next() {
		var name, value string
		var expiry int64
		if err := rows.Scan(&name, &value, &expiry); err != nil {
			return fmt.Errorf("failed to read Firefox cookie: %w", err)
		}
		expiresAt := time.Unix(expiry, 0).UTC()
		if expiry <= 0 || !expiresAt.After(time.Now()) {
			continue
		}
		if earliestExpiry.IsZero() || expiresAt.Before(earliestExpiry) {
			earliestExpiry = expiresAt
		}
		switch name {
		case "hglogin":
			tokens.HGLogin = value
		case "X-Hourglass-XSRF-Token":
			tokens.XSRFToken = value
		}
	}
	tokens.ExpiresAt = earliestExpiry
	if err := rows.Err(); err != nil {
		return fmt.Errorf("failed to read Firefox cookies: %w", err)
	}
	if !tokens.IsUsable() {
		return errors.New("firefox profile does not contain required Hourglass cookies")
	}
	if err := writeTokensAtomically(tokensOut, tokens); err != nil {
		return err
	}
	fmt.Fprintln(c.stdout, "Firefox cookies exported to token file")
	return nil
}

func writeTokensAtomically(path string, tokens webauthn.AuthTokens) error {
	dir := filepath.Dir(filepath.Clean(path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create token directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("failed to secure token directory: %w", err)
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("failed to encode tokens: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tokens-*")
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to secure token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close token file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Clean(path)); err != nil {
		return fmt.Errorf("failed to install token file: %w", err)
	}
	return nil
}

func parseFirefoxBootstrapArgs(args []string) (string, string, error) {
	var profile, tokensOut string
	for i := 0; i < len(args); {
		value, next, err := takeFlagValue(args, i, args[i])
		if err != nil {
			return "", "", err
		}
		switch args[i] {
		case "--profile":
			profile = value
		case "--tokens-out":
			tokensOut = value
		default:
			return "", "", fmt.Errorf("unknown flag %s", args[i])
		}
		i = next
	}
	return profile, tokensOut, nil
}

func (c *CLI) runAuth(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(c.stderr, "Usage: hourglass auth <status|login>\n")
		return errors.New("no auth subcommand provided")
	}
	switch args[0] {
	case "status":
		return c.AuthStatus()
	case "login":
		return c.AuthLogin()
	case "bootstrap-firefox":
		profile, tokensOut, err := parseFirefoxBootstrapArgs(args[1:])
		if err != nil {
			return err
		}
		return c.BootstrapFirefox(profile, tokensOut)
	default:
		fmt.Fprintf(c.stderr, "Unknown auth subcommand: %s\n", args[0])
		fmt.Fprintf(c.stderr, "Usage: hourglass auth <status|login|bootstrap-firefox>\n")
		return errors.New("unknown auth subcommand")
	}
}

func (c *CLI) runUsers(args []string) error {
	jsonFlag := false
	for _, arg := range args {
		if arg == flagJSON {
			jsonFlag = true
		}
	}
	if !jsonFlag {
		fmt.Fprintf(c.stderr, "Usage: hourglass users --json\n")
		return errors.New("missing --json flag")
	}
	return c.Users()
}

type scheduleArgs struct {
	subcommand string
	action     string
	file       string
	fromDate   string
	toDate     string
	lgroup     int
	notifType  string
	apply      bool
}

func parseScheduleArgs(args []string) (scheduleArgs, error) {
	var parsed scheduleArgs
	for i := 0; i < len(args); {
		switch args[i] {
		case "wm", "privileges", "assign", "unassign", "check-rules", "mm", "av", "notifications", "cart":
			parsed.subcommand = args[i]
			i++
			if parsed.subcommand == "cart" && i < len(args) && !strings.HasPrefix(args[i], "--") {
				parsed.action = args[i]
				i++
			}
		case "--file":
			val, next, err := takeFlagValue(args, i, "--file")
			if err != nil {
				return parsed, err
			}
			parsed.file = val
			i = next
		case "--from":
			val, next, err := takeFlagValue(args, i, "--from")
			if err != nil {
				return parsed, err
			}
			parsed.fromDate = val
			i = next
		case "--to":
			val, next, err := takeFlagValue(args, i, "--to")
			if err != nil {
				return parsed, err
			}
			parsed.toDate = val
			i = next
		case "--lgroup":
			val, next, err := takeFlagValue(args, i, "--lgroup")
			if err != nil {
				return parsed, err
			}
			if _, err := fmt.Sscanf(val, "%d", &parsed.lgroup); err != nil {
				parsed.lgroup = 0
			}
			i = next
		case "--type":
			val, next, err := takeFlagValue(args, i, "--type")
			if err != nil {
				return parsed, err
			}
			parsed.notifType = val
			i = next
		case "--apply":
			parsed.apply = true
			i++
		case flagJSON:
			i++
		default:
			i++
		}
	}
	return parsed, nil
}

func takeFlagValue(args []string, i int, name string) (string, int, error) {
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("missing value for %s", name)
	}
	return args[i+1], i + 2, nil
}

func (c *CLI) runSchedule(args []string) error {
	parsed, err := parseScheduleArgs(args)
	if err != nil {
		return err
	}
	return c.dispatchSchedule(parsed)
}

func (c *CLI) dispatchSchedule(parsed scheduleArgs) error {
	switch parsed.subcommand {
	case "wm":
		if parsed.fromDate == "" || parsed.toDate == "" {
			fmt.Fprintf(c.stderr, "Usage: hourglass schedule wm --from YYYY-MM-DD --to YYYY-MM-DD --json\n")
			return errors.New("missing required --from/--to for wm")
		}
		return c.ScheduleWM(parsed.fromDate, parsed.toDate)
	case "privileges":
		return c.Privileges()
	case cmdAssign:
		return c.runAssignLike(cmdAssign, parsed.apply, c.Assign)
	case "unassign":
		return c.runAssignLike("unassign", parsed.apply, c.Unassign)
	case "check-rules":
		return c.CheckRules()
	case "mm":
		return c.runScheduleMM(parsed)
	case "av":
		return c.runScheduleAV(parsed)
	case "notifications":
		return c.runScheduleNotifications(parsed)
	case "cart":
		return c.runCartSchedule(parsed)
	default:
		fmt.Fprintf(c.stderr, "Usage: hourglass schedule <wm|privileges|assign|unassign|check-rules|mm|av|notifications> [flags]\n")
		return errors.New("unknown schedule subcommand")
	}
}

func (c *CLI) runAssignLike(name string, apply bool, fn func() error) error {
	if !apply {
		fmt.Fprintf(c.stderr, "Info: %s running in dry-run mode (use --apply to attempt actual operation)\n", name)
	}
	return fn()
}

func (c *CLI) runScheduleMM(parsed scheduleArgs) error {
	if parsed.fromDate == "" || parsed.toDate == "" {
		fmt.Fprintf(c.stderr, "Usage: hourglass schedule mm --from YYYY-MM-DD --to YYYY-MM-DD [--lgroup N] --json\n")
		return errors.New("missing required flags")
	}
	return c.ScheduleMeetings(parsed.fromDate, parsed.toDate, parsed.lgroup)
}

func (c *CLI) runScheduleAV(parsed scheduleArgs) error {
	if parsed.fromDate == "" || parsed.toDate == "" {
		fmt.Fprintf(c.stderr, "Usage: hourglass schedule av --from YYYY-MM-DD --to YYYY-MM-DD --json\n")
		return errors.New("missing required flags")
	}
	return c.ScheduleAVAttendants(parsed.fromDate, parsed.toDate)
}

// ImportCartSchedule validates a public witnessing spreadsheet and prints the
// slots it would create. It never writes to Hourglass.
func (c *CLI) ImportCartSchedule(file string, apply bool) error {
	if file == "" {
		return exitCode2("missing --file with the spreadsheet path")
	}
	source, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	rows, err := cartimport.Parse(source)
	if err != nil {
		return err
	}
	start, end := rows[0].Date, rows[0].Date
	for _, row := range rows {
		if row.Date.Before(start) {
			start = row.Date
		}
		if row.Date.After(end) {
			end = row.Date
		}
	}

	locations, err := c.client.GetPWLocations()
	if err != nil {
		return err
	}
	users, err := c.client.GetUsers()
	if err != nil {
		return err
	}
	existing, err := c.client.GetPWSchedule(start.Format(time.DateOnly), end.Format(time.DateOnly))
	if err != nil {
		return err
	}

	plan := cartimport.Build(rows, locations, users, existing)
	if err := c.printCartPlan(plan, apply); err != nil {
		return err
	}
	return nil
}

func (c *CLI) printCartPlan(plan cartimport.Plan, apply bool) error {
	problems := make([]string, 0, len(plan.Problems))
	for _, problem := range plan.Problems {
		problems = append(problems, problem.String())
	}
	result := map[string]any{
		"entries":  plan.Entries,
		"problems": problems,
		"applied":  false,
	}
	if err := marshalJSONTo(c.stdout, result); err != nil {
		return err
	}
	if len(plan.Problems) > 0 {
		return fmt.Errorf("planilha com %d problema(s); nada será gravado", len(plan.Problems))
	}
	if apply {
		return errors.New("gravação ainda não habilitada: contrato de escrita do Hourglass não confirmado")
	}
	return nil
}

func marshalJSONTo(w io.Writer, value any) error {
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}

func (c *CLI) runCartSchedule(parsed scheduleArgs) error {
	switch parsed.action {
	case "import":
		return c.ImportCartSchedule(parsed.file, parsed.apply)
	case "copy":
		return c.CopyCartMonth(parsed.fromDate, parsed.toDate, parsed.apply)
	case "export":
		return c.ExportCartMonth(parsed.fromDate, parsed.toDate, parsed.file)
	case "sync":
		return c.SyncCartSheet(parsed.file, parsed.apply)
	default:
		fmt.Fprintf(c.stderr, "Usage: hourglass schedule cart <import --file <planilha.csv> | copy --from YYYY-MM --to YYYY-MM> [--apply] --json\n")
		return errors.New("unknown cart subcommand")
	}
}

// CopyCartMonth previews the target month built from the source month, keeping
// weekday, time of day, location and publishers.
func (c *CLI) CopyCartMonth(fromMonth, toMonth string, apply bool) error {
	start, err := time.Parse("2006-01", fromMonth)
	if err != nil {
		return exitCode2("--from must be YYYY-MM")
	}
	target, err := time.Parse("2006-01", toMonth)
	if err != nil {
		return exitCode2("--to must be YYYY-MM")
	}
	source, err := c.client.GetPWSchedule(start.Format(time.DateOnly), start.AddDate(0, 1, -1).Format(time.DateOnly))
	if err != nil {
		return err
	}
	existing, err := c.client.GetPWSchedule(target.Format(time.DateOnly), target.AddDate(0, 1, -1).Format(time.DateOnly))
	if err != nil {
		return err
	}

	plan := cartimport.CopyMonth(source, target, existing)
	problems := make([]string, 0, len(plan.Problems))
	for _, problem := range plan.Problems {
		problems = append(problems, problem.String())
	}
	result := map[string]any{
		"from":          fromMonth,
		"to":            toMonth,
		"source_slots":  len(source),
		"target_before": len(existing),
		"entries":       plan.Entries,
		"problems":      problems,
		"applied":       false,
	}
	if !apply {
		return marshalJSONTo(c.stdout, result)
	}
	if len(plan.Entries) == 0 {
		return marshalJSONTo(c.stdout, result)
	}

	created := 0
	for _, entry := range plan.Entries {
		draft := hourglass.PWSlotDraft{
			LocationID: entry.LocationID,
			Datetime:   entry.Datetime.Format(time.RFC3339),
			Duration:   fmt.Sprintf("%.2f", entry.DurationHours),
		}
		if entry.PublisherID != 0 {
			publisher := entry.PublisherID
			draft.AssigneeID = &publisher
		}
		if _, err := c.client.UpsertPWSlot(draft); err != nil {
			return fmt.Errorf("falha ao gravar a %dª de %d designações: %w", created+1, len(plan.Entries), err)
		}
		created++
	}
	after, err := c.client.GetPWSchedule(target.Format(time.DateOnly), target.AddDate(0, 1, -1).Format(time.DateOnly))
	if err != nil {
		return err
	}
	result["applied"] = true
	result["created"] = created
	result["target_after"] = len(after)
	return marshalJSONTo(c.stdout, result)
}

// ExportCartMonth writes the live month to a spreadsheet, one row per location
// and hour, with up to two publishers.
func (c *CLI) ExportCartMonth(fromMonth, toMonth, out string) error {
	start, err := time.Parse("2006-01", fromMonth)
	if err != nil {
		return exitCode2("--from must be YYYY-MM")
	}
	end, err := time.Parse("2006-01", toMonth)
	if err != nil {
		return exitCode2("--to must be YYYY-MM")
	}
	if out == "" {
		return exitCode2("missing --file with the output path")
	}
	slots, err := c.client.GetPWSchedule(start.Format(time.DateOnly), end.AddDate(0, 1, -1).Format(time.DateOnly))
	if err != nil {
		return err
	}
	locations, err := c.client.GetPWLocations()
	if err != nil {
		return err
	}
	users, err := c.client.GetUsers()
	if err != nil {
		return err
	}
	rows, problems := cartimport.ToSheet(slots, locations, users)
	if err := writeSheet(out, rows); err != nil {
		return err
	}
	return marshalJSONTo(c.stdout, map[string]any{"file": out, "rows": len(rows), "problems": problems})
}

// SyncCartSheet compares the spreadsheet with the live month and, with --apply,
// creates the missing slots and deletes the ones removed from the sheet.
func (c *CLI) SyncCartSheet(file string, apply bool) error {
	if file == "" {
		return exitCode2("missing --file with the spreadsheet path")
	}
	csvPath, cleanup, err := sheetAsCSV(file)
	if err != nil {
		return err
	}
	defer cleanup()

	source, err := os.Open(csvPath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	rows, err := cartimport.Parse(source)
	if err != nil {
		return err
	}
	start, end := rows[0].Date, rows[0].Date
	for _, row := range rows {
		if row.Date.Before(start) {
			start = row.Date
		}
		if row.Date.After(end) {
			end = row.Date
		}
	}
	locations, err := c.client.GetPWLocations()
	if err != nil {
		return err
	}
	users, err := c.client.GetUsers()
	if err != nil {
		return err
	}
	live, err := c.client.GetPWSchedule(start.Format(time.DateOnly), end.Format(time.DateOnly))
	if err != nil {
		return err
	}
	desired, problems := cartimport.Desired(rows, locations, users)
	diff := cartimport.Compare(desired, live)
	result := map[string]any{
		"create":   len(diff.Create),
		"delete":   len(diff.Delete),
		"problems": problems,
		"applied":  false,
	}
	if !apply || diff.IsEmpty() {
		return marshalJSONTo(c.stdout, result)
	}
	for _, entry := range diff.Create {
		draft := hourglass.PWSlotDraft{
			LocationID: entry.LocationID,
			Datetime:   entry.Datetime.Format(time.RFC3339),
			Duration:   fmt.Sprintf("%.2f", entry.DurationHours),
		}
		if entry.PublisherID != 0 {
			publisher := entry.PublisherID
			draft.AssigneeID = &publisher
		}
		if _, err := c.client.UpsertPWSlot(draft); err != nil {
			return fmt.Errorf("falha ao criar designação: %w", err)
		}
	}
	for _, slot := range diff.Delete {
		if err := c.client.DeletePWSlot(slot.ID); err != nil {
			return fmt.Errorf("falha ao remover designação: %w", err)
		}
	}
	result["applied"] = true
	return marshalJSONTo(c.stdout, result)
}

func writeSheet(path string, rows []cartimport.ExportRow) error {
	temporary, err := os.CreateTemp("", "cart-export-*.csv")
	if err != nil {
		return err
	}
	csvPath := temporary.Name()
	defer func() { _ = os.Remove(csvPath) }()

	writer := csv.NewWriter(temporary)
	_ = writer.Write(cartimport.Columns)
	for _, row := range rows {
		_ = writer.Write([]string{row.Location, row.Date, row.Start, row.End, row.Publisher1, row.Publisher2})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(path), ".xlsx") {
		return convertToXLSX(csvPath, path)
	}
	return copyFile(csvPath, path)
}

func convertToXLSX(source, target string) error {
	dir, err := os.MkdirTemp("", "cart-xlsx-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "soffice", "--headless", "--convert-to", "xlsx", "--outdir", dir, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("soffice failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	base := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	return copyFile(filepath.Join(dir, base+".xlsx"), target)
}

func sheetAsCSV(path string) (string, func(), error) {
	if !strings.HasSuffix(strings.ToLower(path), ".xlsx") {
		return path, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "cart-sheet-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "soffice", "--headless", "--convert-to", "csv", "--outdir", dir, path)
	if output, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("soffice failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return filepath.Join(dir, base+".csv"), cleanup, nil
}

func copyFile(source, target string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, content, 0o600)
}

func (c *CLI) runScheduleNotifications(parsed scheduleArgs) error {
	if parsed.fromDate == "" || parsed.toDate == "" {
		fmt.Fprintf(c.stderr, "Usage: hourglass schedule notifications --from YYYY-MM-DD --to YYYY-MM-DD [--type mm|ava|fm|pubwit] --json\n")
		return errors.New("missing required flags")
	}
	return c.ScheduleNotifications(parsed.fromDate, parsed.toDate, parsed.notifType)
}

// blocklistTransport refuses mutating HTTP methods and send-like paths unless allowSend is set.
// allowAssign permits a single assignee-only PUT to scheduling/av_attendant/{id} and never send/publish.
type blocklistTransport struct {
	base        http.RoundTripper
	allowSend   bool
	allowAssign bool
	allowCart   bool
}

var avAssigneeWritePath = regexp.MustCompile(`(?i)(?:^|/)scheduling/av_attendant/\d+/?$`)

func (t *blocklistTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if isSendLikePath(req.URL.Path) && !t.allowSend {
		return nil, fmt.Errorf("send operation blocked: %s %s", req.Method, req.URL.Path)
	}

	switch req.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		if t.allowSend {
			break
		}
		if t.allowAssign && isAssigneeOnlyWrite(req) {
			break
		}
		if t.allowCart && isCartWrite(req) {
			break
		}
		return nil, fmt.Errorf("send operations blocked: %s %s", req.Method, req.URL.Path)
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

var (
	cartWritePath  = regexp.MustCompile(`(?i)(?:^|/)(?:scheduling/pw/schedule|smpw/schedules)/?$`)
	cartDeletePath = regexp.MustCompile(`(?i)(?:^|/)scheduling/pw/schedule/\d+/?$`)
)

// isCartWrite allows the public witnessing slot upsert and the single-slot delete,
// never a send or notify call.
func isCartWrite(req *http.Request) bool {
	switch {
	case req.Method == http.MethodPut && cartWritePath.MatchString(req.URL.Path):
	case req.Method == http.MethodDelete && cartDeletePath.MatchString(req.URL.Path):
	default:
		return false
	}
	q := strings.ToLower(req.URL.RawQuery)
	return !strings.Contains(q, "publish") && !strings.Contains(q, "notify") && !strings.Contains(q, "send")
}

func isAssigneeOnlyWrite(req *http.Request) bool {
	if req.Method != http.MethodPut {
		return false
	}
	if !avAssigneeWritePath.MatchString(req.URL.Path) {
		return false
	}
	// Reject publish/notify query knobs even on the assignee path.
	q := strings.ToLower(req.URL.RawQuery)
	if strings.Contains(q, "publish") || strings.Contains(q, "notify") || strings.Contains(q, "send") {
		return false
	}
	return true
}

func isSendLikePath(rawPath string) bool {
	path := strings.ToLower(strings.Trim(rawPath, "/"))
	if path == "" {
		return false
	}

	// Legitimate read endpoint: GET /scheduling/notifications/...
	if path == "scheduling/notifications" || strings.HasPrefix(path, "scheduling/notifications/") {
		return false
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		switch segment {
		case "notify", "send", "publish", "email":
			return true
		case "notification":
			if i+1 < len(segments) && segments[i+1] == "send" {
				return true
			}
		}
	}
	return false
}

// run executes the CLI and returns a process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cli := newCLI(stdout, stderr, os.Getenv)
	if stop := cli.bootstrapWebAuthn(); stop != nil {
		defer stop()
	}
	if err := cli.dispatch(args); err != nil {
		var code2 *exitCode2Error
		if errors.As(err, &code2) {
			fmt.Fprintf(stderr, "Error: %s\n", code2.msg)
			return 2
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}
