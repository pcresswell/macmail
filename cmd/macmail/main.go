package main

import (
	"database/sql"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
)

var (
	version = "dev" // set via -ldflags at build time
	mailDir string
	dbPath  string
)

// DBOpener is an interface for opening database connections
type DBOpener func() (*sql.DB, error)

// EmailReader is an interface for reading email files
type EmailReader func(path string) ([]byte, error)

// Default implementations
var (
	defaultDBOpener    DBOpener    = func() (*sql.DB, error) { return sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", dbPath)) }
	defaultEmailReader EmailReader = os.ReadFile
)

// App holds the application dependencies
type App struct {
	OpenDB    DBOpener
	ReadEmail EmailReader
	Output    io.Writer
	MailDir   string
}

// NewApp creates a new App with default dependencies
func NewApp() *App {
	return &App{
		OpenDB:    defaultDBOpener,
		ReadEmail: defaultEmailReader,
		Output:    os.Stdout,
		MailDir:   mailDir,
	}
}

func init() {
	homeDir, _ := os.UserHomeDir()
	mailDir = filepath.Join(homeDir, "Library", "Mail", "V10")
	dbPath = filepath.Join(mailDir, "MailData", "Envelope Index")
}

func getDB() (*sql.DB, error) {
	return defaultDBOpener()
}

func main() {
	app := NewApp()
	if err := run(app); err != nil {
		os.Exit(1)
	}
}

func run(app *App) error {
	rootCmd := buildRootCmd(app)
	return rootCmd.Execute()
}

func buildRootCmd(app *App) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     "macmail",
		Short:   "Read emails from Mac Mail's local database",
		Long:    "A command-line tool to query and read emails stored locally by Mac Mail.",
		Version: version,
	}

	// mailboxes command
	mailboxesCmd := &cobra.Command{
		Use:   "mailboxes",
		Short: "List all mailboxes",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.RunMailboxes()
		},
	}

	// list command
	var listLimit int
	var listMailbox int
	var listUnread bool
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List recent emails",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.RunList(listLimit, listMailbox, listUnread)
		},
	}
	listCmd.Flags().IntVarP(&listLimit, "limit", "n", 20, "Number of emails to show")
	listCmd.Flags().IntVarP(&listMailbox, "mailbox", "m", 0, "Filter by mailbox ID")
	listCmd.Flags().BoolVarP(&listUnread, "unread", "u", false, "Show only unread emails")

	// search command
	var searchLimit int
	searchCmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search emails by subject or sender",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.RunSearch(args[0], searchLimit)
		},
	}
	searchCmd.Flags().IntVarP(&searchLimit, "limit", "n", 20, "Maximum results")

	// read command
	readCmd := &cobra.Command{
		Use:   "read <id>",
		Short: "Read an email by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid email ID: %s", args[0])
			}
			return app.RunRead(id)
		},
	}

	// unread command
	unreadCmd := &cobra.Command{
		Use:   "unread [limit]",
		Short: "Show unread emails",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit := 0 // 0 means no limit
			if len(args) == 1 {
				var err error
				limit, err = strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid limit: %s", args[0])
				}
			}
			return app.RunUnread(limit)
		},
	}

	rootCmd.AddCommand(mailboxesCmd, listCmd, searchCmd, readCmd, unreadCmd)

	return rootCmd
}

func (a *App) RunMailboxes() error {
	db, err := a.OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT ROWID, url, total_count, unread_count
		FROM mailboxes
		WHERE total_count > 0
		ORDER BY url
	`)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	fmt.Fprintf(a.Output, "%-5s %-50s %8s %8s\n", "ID", "Mailbox", "Total", "Unread")
	fmt.Fprintln(a.Output, strings.Repeat("-", 75))

	for rows.Next() {
		var id int
		var urlStr string
		var total, unread int

		if err := rows.Scan(&id, &urlStr, &total, &unread); err != nil {
			return err
		}

		// Extract mailbox name from URL
		name := urlStr
		if idx := strings.LastIndex(urlStr, "/"); idx != -1 {
			name = urlStr[idx+1:]
		}
		// URL decode
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}

		fmt.Fprintf(a.Output, "%-5d %-50s %8d %8d\n", id, truncate(name, 50), total, unread)
	}

	return nil
}

func (a *App) RunList(limit, mailboxID int, unreadOnly bool) error {
	db, err := a.OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `
		SELECT
			m.ROWID,
			m.date_received,
			COALESCE(a.address, '') as from_email,
			COALESCE(a.comment, '') as from_name,
			COALESCE(s.subject, '(no subject)') as subject,
			m.read
		FROM messages m
		LEFT JOIN subjects s ON m.subject = s.ROWID
		LEFT JOIN sender_addresses sa ON sa.sender = m.sender
		LEFT JOIN addresses a ON sa.address = a.ROWID
		WHERE 1=1
	`
	args := []interface{}{}

	if mailboxID > 0 {
		query += " AND m.mailbox = ?"
		args = append(args, mailboxID)
	}

	if unreadOnly {
		query += " AND m.read = 0"
	}

	query += " ORDER BY m.date_received DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int
		var dateReceived int64
		var fromEmail, fromName, subject string
		var read int

		if err := rows.Scan(&id, &dateReceived, &fromEmail, &fromName, &subject, &read); err != nil {
			return err
		}

		status := "UNREAD"
		if read == 1 {
			status = "read"
		}

		sender := fromName
		if sender == "" {
			sender = fromEmail
		}
		if sender == "" {
			sender = "(unknown)"
		}

		date := time.Unix(dateReceived, 0).Format("2006-01-02 15:04")

		fmt.Fprintf(a.Output, "\n[%d] %s\n", id, status)
		fmt.Fprintf(a.Output, "  Date: %s\n", date)
		fmt.Fprintf(a.Output, "  From: %s\n", sender)
		fmt.Fprintf(a.Output, "  Subj: %s\n", truncate(subject, 80))
	}

	return nil
}

func (a *App) RunUnread(limit int) error {
	db, err := a.OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `
		SELECT
			m.ROWID,
			m.date_received,
			COALESCE(a.address, '') as from_email,
			COALESCE(a.comment, '') as from_name,
			COALESCE(s.subject, '(no subject)') as subject
		FROM messages m
		LEFT JOIN subjects s ON m.subject = s.ROWID
		LEFT JOIN sender_addresses sa ON sa.sender = m.sender
		LEFT JOIN addresses a ON sa.address = a.ROWID
		WHERE m.read = 0
		ORDER BY m.date_received DESC
	`

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int
		var dateReceived int64
		var fromEmail, fromName, subject string

		if err := rows.Scan(&id, &dateReceived, &fromEmail, &fromName, &subject); err != nil {
			return err
		}

		count++

		sender := fromName
		if sender == "" {
			sender = fromEmail
		}
		if sender == "" {
			sender = "(unknown)"
		}

		date := time.Unix(dateReceived, 0).Format("2006-01-02 15:04")

		fmt.Fprintf(a.Output, "\n[%d] UNREAD\n", id)
		fmt.Fprintf(a.Output, "  Date: %s\n", date)
		fmt.Fprintf(a.Output, "  From: %s\n", sender)
		fmt.Fprintf(a.Output, "  Subj: %s\n", truncate(subject, 80))
	}

	fmt.Fprintf(a.Output, "\n%d unread emails\n", count)
	return nil
}

func (a *App) RunSearch(query string, limit int) error {
	db, err := a.OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	searchPattern := "%" + query + "%"

	rows, err := db.Query(`
		SELECT
			m.ROWID,
			m.date_received,
			COALESCE(a.address, '') as from_email,
			COALESCE(a.comment, '') as from_name,
			COALESCE(s.subject, '(no subject)') as subject,
			m.read
		FROM messages m
		LEFT JOIN subjects s ON m.subject = s.ROWID
		LEFT JOIN sender_addresses sa ON sa.sender = m.sender
		LEFT JOIN addresses a ON sa.address = a.ROWID
		WHERE s.subject LIKE ? OR a.address LIKE ? OR a.comment LIKE ?
		ORDER BY m.date_received DESC
		LIMIT ?
	`, searchPattern, searchPattern, searchPattern, limit)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int
		var dateReceived int64
		var fromEmail, fromName, subject string
		var read int

		if err := rows.Scan(&id, &dateReceived, &fromEmail, &fromName, &subject, &read); err != nil {
			return err
		}

		count++

		status := "UNREAD"
		if read == 1 {
			status = "read"
		}

		sender := fromName
		if sender == "" {
			sender = fromEmail
		}
		if sender == "" {
			sender = "(unknown)"
		}

		date := time.Unix(dateReceived, 0).Format("2006-01-02 15:04")

		fmt.Fprintf(a.Output, "\n[%d] %s\n", id, status)
		fmt.Fprintf(a.Output, "  Date: %s\n", date)
		fmt.Fprintf(a.Output, "  From: %s\n", sender)
		fmt.Fprintf(a.Output, "  Subj: %s\n", truncate(subject, 80))
	}

	fmt.Fprintf(a.Output, "\nFound %d results for '%s'\n", count, query)
	return nil
}

func (a *App) RunRead(id int) error {
	db, err := a.OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	var dateReceived int64
	var fromEmail, fromName, subject, mailboxURL string

	err = db.QueryRow(`
		SELECT
			m.date_received,
			COALESCE(a.address, '') as from_email,
			COALESCE(a.comment, '') as from_name,
			COALESCE(s.subject, '(no subject)') as subject,
			mb.url as mailbox
		FROM messages m
		LEFT JOIN subjects s ON m.subject = s.ROWID
		LEFT JOIN sender_addresses sa ON sa.sender = m.sender
		LEFT JOIN addresses a ON sa.address = a.ROWID
		LEFT JOIN mailboxes mb ON m.mailbox = mb.ROWID
		WHERE m.ROWID = ?
	`, id).Scan(&dateReceived, &fromEmail, &fromName, &subject, &mailboxURL)

	if err == sql.ErrNoRows {
		return fmt.Errorf("email %d not found", id)
	}
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}

	sender := fromName
	if sender == "" {
		sender = fromEmail
	}

	date := time.Unix(dateReceived, 0).Format("2006-01-02 15:04:05")

	fmt.Fprintln(a.Output, strings.Repeat("=", 70))
	fmt.Fprintf(a.Output, "ID:      %d\n", id)
	fmt.Fprintf(a.Output, "Date:    %s\n", date)
	fmt.Fprintf(a.Output, "From:    %s <%s>\n", sender, fromEmail)
	fmt.Fprintf(a.Output, "Subject: %s\n", subject)
	fmt.Fprintln(a.Output, strings.Repeat("=", 70))

	// Find and read the emlx file
	emlxPath := a.getEmlxPath(id, mailboxURL)
	if emlxPath == "" {
		fmt.Fprintln(a.Output, "\nCould not find email file")
		return nil
	}

	content, err := a.ReadEmail(emlxPath)
	if err != nil {
		return fmt.Errorf("failed to read email file: %w", err)
	}

	body := extractPlainTextBody(string(content))

	fmt.Fprintln(a.Output, "\nBODY:")
	fmt.Fprintln(a.Output, strings.Repeat("-", 70))

	lines := strings.Split(body, "\n")
	maxLines := 100
	if len(lines) > maxLines {
		fmt.Fprintln(a.Output, strings.Join(lines[:maxLines], "\n"))
		fmt.Fprintf(a.Output, "\n... [%d more lines]\n", len(lines)-maxLines)
	} else {
		fmt.Fprintln(a.Output, body)
	}

	return nil
}

func (a *App) getEmlxPath(rowid int, mailboxURL string) string {
	// Parse mailbox URL: ews://UUID/MailboxName or imap://UUID/INBOX
	urlStr := strings.TrimPrefix(mailboxURL, "ews://")
	urlStr = strings.TrimPrefix(urlStr, "imap://")

	parts := strings.SplitN(urlStr, "/", 2)
	if len(parts) < 2 {
		return ""
	}

	accountUUID := parts[0]
	mailboxName := parts[1]

	// URL decode mailbox name
	if decoded, err := url.QueryUnescape(mailboxName); err == nil {
		mailboxName = decoded
	}

	// Get first 3 digits for directory path (reversed)
	rowidStr := strconv.Itoa(rowid)
	if len(rowidStr) < 3 {
		return ""
	}
	d1 := string(rowidStr[2]) // third digit
	d2 := string(rowidStr[1]) // second digit
	d3 := string(rowidStr[0]) // first digit

	// Find the mailbox directory
	mailboxDir := filepath.Join(a.MailDir, accountUUID, mailboxName+".mbox")

	entries, err := os.ReadDir(mailboxDir)
	if err != nil {
		return ""
	}

	// Find the UUID subdirectory containing Data
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			emlxPath := filepath.Join(mailboxDir, entry.Name(), "Data", d1, d2, d3, "Messages", fmt.Sprintf("%d.emlx", rowid))
			if _, err := os.Stat(emlxPath); err == nil {
				return emlxPath
			}
		}
	}

	return ""
}

// Keep the standalone function for backward compatibility in tests
func getEmlxPath(rowid int, mailboxURL string) string {
	app := &App{MailDir: mailDir}
	return app.getEmlxPath(rowid, mailboxURL)
}

func extractPlainTextBody(emlxContent string) string {
	// Skip the byte count on the first line
	lines := strings.SplitN(emlxContent, "\n", 2)
	if len(lines) < 2 {
		return ""
	}

	emailContent := lines[1]

	// Remove trailing Apple plist
	if idx := strings.Index(emailContent, "<?xml version"); idx != -1 {
		emailContent = emailContent[:idx]
	}

	// Parse the email
	msg, err := mail.ReadMessage(strings.NewReader(emailContent))
	if err != nil {
		// Fallback: find body after headers
		return extractBodyFallback(emailContent)
	}

	contentType := msg.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return extractBodyFallback(emailContent)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		return extractMultipartBody(msg.Body, params["boundary"])
	}

	// Single part message
	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return extractBodyFallback(emailContent)
	}

	return string(body)
}

func extractMultipartBody(body io.Reader, boundary string) string {
	if boundary == "" {
		return ""
	}

	mr := multipart.NewReader(body, boundary)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		contentType := part.Header.Get("Content-Type")
		mediaType, params, _ := mime.ParseMediaType(contentType)

		// Recursively handle nested multipart
		if strings.HasPrefix(mediaType, "multipart/") {
			result := extractMultipartBody(part, params["boundary"])
			if result != "" {
				return result
			}
			continue
		}

		// Prefer text/plain
		if mediaType == "text/plain" || strings.HasPrefix(mediaType, "text/plain;") {
			content, err := io.ReadAll(part)
			if err == nil {
				return string(content)
			}
		}
	}

	return ""
}

func extractBodyFallback(content string) string {
	lines := strings.Split(content, "\n")
	inBody := false
	var bodyLines []string

	for _, line := range lines {
		if inBody {
			if strings.HasPrefix(strings.TrimSpace(line), "<?xml") {
				break
			}
			bodyLines = append(bodyLines, line)
		} else if strings.TrimSpace(line) == "" {
			inBody = true
		}
	}

	return strings.Join(bodyLines, "\n")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
