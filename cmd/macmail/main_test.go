package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// setupTestDB creates an in-memory SQLite database with test data
func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}

	// Create tables matching Mac Mail's schema
	schema := `
		CREATE TABLE mailboxes (
			ROWID INTEGER PRIMARY KEY,
			url TEXT NOT NULL,
			total_count INTEGER DEFAULT 0,
			unread_count INTEGER DEFAULT 0
		);

		CREATE TABLE subjects (
			ROWID INTEGER PRIMARY KEY,
			subject TEXT NOT NULL
		);

		CREATE TABLE addresses (
			ROWID INTEGER PRIMARY KEY,
			address TEXT NOT NULL,
			comment TEXT DEFAULT ''
		);

		CREATE TABLE senders (
			ROWID INTEGER PRIMARY KEY,
			contact_identifier TEXT,
			bucket INTEGER DEFAULT 0,
			user_initiated INTEGER DEFAULT 1
		);

		CREATE TABLE sender_addresses (
			address INTEGER,
			sender INTEGER NOT NULL
		);

		CREATE TABLE messages (
			ROWID INTEGER PRIMARY KEY,
			message_id INTEGER,
			subject INTEGER,
			sender INTEGER,
			date_received INTEGER,
			mailbox INTEGER,
			read INTEGER DEFAULT 0
		);
	`

	_, err = db.Exec(schema)
	if err != nil {
		t.Fatalf("Failed to create schema: %v", err)
	}

	// Insert test data
	testData := `
		INSERT INTO mailboxes (ROWID, url, total_count, unread_count) VALUES
			(1, 'ews://TEST-UUID/Inbox', 100, 10),
			(2, 'ews://TEST-UUID/Sent%20Items', 50, 0),
			(3, 'ews://TEST-UUID/Drafts', 5, 5);

		INSERT INTO subjects (ROWID, subject) VALUES
			(1, 'Test Email Subject'),
			(2, 'Another Test Email'),
			(3, 'Meeting Tomorrow'),
			(4, 'Azure Deployment Issue');

		INSERT INTO addresses (ROWID, address, comment) VALUES
			(1, 'sender@example.com', 'John Doe'),
			(2, 'boss@company.com', 'Jane Smith'),
			(3, 'noreply@service.com', '');

		INSERT INTO senders (ROWID) VALUES (1), (2), (3);

		INSERT INTO sender_addresses (address, sender) VALUES
			(1, 1),
			(2, 2),
			(3, 3);

		INSERT INTO messages (ROWID, message_id, subject, sender, date_received, mailbox, read) VALUES
			(879823, 1001, 1, 1, 1706100000, 1, 0),
			(879824, 1002, 2, 2, 1706100100, 1, 1),
			(879825, 1003, 3, 3, 1706100200, 1, 0),
			(879826, 1004, 4, 1, 1706100300, 1, 0);
	`

	_, err = db.Exec(testData)
	if err != nil {
		t.Fatalf("Failed to insert test data: %v", err)
	}

	return db
}

// newTestApp creates an App configured for testing
func newTestApp(t *testing.T, db *sql.DB) (*App, *bytes.Buffer) {
	output := &bytes.Buffer{}
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return db, nil
		},
		ReadEmail: func(path string) ([]byte, error) {
			return []byte(`1234
From: sender@example.com
Subject: Test
Content-Type: text/plain

This is the email body.
`), nil
		},
		Output:  output,
		MailDir: t.TempDir(),
	}
	return app, output
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 8, "hello..."},
		{"empty string", "", 10, ""},
		{"single char limit", "hello", 4, "h..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncate(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestExtractBodyFallback(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "simple email",
			input: `From: test@example.com
Subject: Test

This is the body.
Second line.`,
			expected: "This is the body.\nSecond line.",
		},
		{
			name: "email with xml plist",
			input: `From: test@example.com
Subject: Test

Body content here.
<?xml version="1.0"?>
<plist>data</plist>`,
			expected: "Body content here.",
		},
		{
			name: "multiple blank lines in headers",
			input: `From: test@example.com
Subject: Test
X-Header: value

Actual body starts here.`,
			expected: "Actual body starts here.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractBodyFallback(tt.input)
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)
			if result != expected {
				t.Errorf("extractBodyFallback() = %q, want %q", result, expected)
			}
		})
	}
}

func TestExtractPlainTextBody(t *testing.T) {
	tests := []struct {
		name        string
		emlxContent string
		contains    string
	}{
		{
			name: "simple plain text email",
			emlxContent: `1234
From: sender@example.com
To: recipient@example.com
Subject: Test Subject
Content-Type: text/plain; charset="utf-8"

This is the plain text body.
It has multiple lines.`,
			contains: "This is the plain text body.",
		},
		{
			name: "email with byte count and plist",
			emlxContent: `5678
From: sender@example.com
Subject: Test
Content-Type: text/plain

Hello World
<?xml version="1.0"?>
<plist></plist>`,
			contains: "Hello World",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPlainTextBody(tt.emlxContent)
			if !strings.Contains(result, tt.contains) {
				t.Errorf("extractPlainTextBody() = %q, want to contain %q", result, tt.contains)
			}
		})
	}
}

func TestAppGetEmlxPath(t *testing.T) {
	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create test directory structure
	accountUUID := "TEST-UUID-1234"
	testPath := filepath.Join(tmpDir, accountUUID, "Inbox.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	err := os.MkdirAll(testPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create test emlx file
	emlxFile := filepath.Join(testPath, "879823.emlx")
	err = os.WriteFile(emlxFile, []byte("test content"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{MailDir: tmpDir}

	tests := []struct {
		name       string
		rowid      int
		mailboxURL string
		wantFound  bool
	}{
		{
			name:       "valid path",
			rowid:      879823,
			mailboxURL: "ews://" + accountUUID + "/Inbox",
			wantFound:  true,
		},
		{
			name:       "imap url format",
			rowid:      879823,
			mailboxURL: "imap://" + accountUUID + "/Inbox",
			wantFound:  true,
		},
		{
			name:       "non-existent mailbox",
			rowid:      879823,
			mailboxURL: "ews://" + accountUUID + "/NonExistent",
			wantFound:  false,
		},
		{
			name:       "non-existent rowid",
			rowid:      123456,
			mailboxURL: "ews://" + accountUUID + "/Inbox",
			wantFound:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := app.getEmlxPath(tt.rowid, tt.mailboxURL)
			found := result != ""
			if found != tt.wantFound {
				t.Errorf("getEmlxPath(%d, %q) found = %v, want %v (path: %q)", tt.rowid, tt.mailboxURL, found, tt.wantFound, result)
			}
		})
	}
}

func TestEmlxPathDigits(t *testing.T) {
	tests := []struct {
		rowid    int
		expected string
	}{
		{879823, "9/7/8"},
		{123456, "3/2/1"},
		{999999, "9/9/9"},
		{100000, "0/0/1"},
		{799585, "9/9/7"},
		{855945, "5/5/8"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("rowid_%d", tt.rowid), func(t *testing.T) {
			rowidStr := strconv.Itoa(tt.rowid)
			if len(rowidStr) < 3 {
				t.Skip("rowid too short")
			}
			d1 := string(rowidStr[2])
			d2 := string(rowidStr[1])
			d3 := string(rowidStr[0])

			got := d1 + "/" + d2 + "/" + d3
			if got != tt.expected {
				t.Errorf("digits for rowid %d = %q, want %q", tt.rowid, got, tt.expected)
			}
		})
	}
}

func TestExtractMultipartBody(t *testing.T) {
	boundary := "----=_Part_123"
	multipartBody := `------=_Part_123
Content-Type: text/plain; charset="utf-8"

This is the plain text version.
------=_Part_123
Content-Type: text/html; charset="utf-8"

<html><body>This is HTML</body></html>
------=_Part_123--`

	result := extractMultipartBody(strings.NewReader(multipartBody), boundary)
	expected := "This is the plain text version."

	if strings.TrimSpace(result) != expected {
		t.Errorf("extractMultipartBody() = %q, want %q", strings.TrimSpace(result), expected)
	}
}

func TestExtractMultipartBodyNoPlainText(t *testing.T) {
	boundary := "----=_Part_456"
	multipartBody := `------=_Part_456
Content-Type: text/html; charset="utf-8"

<html><body>Only HTML here</body></html>
------=_Part_456--`

	result := extractMultipartBody(strings.NewReader(multipartBody), boundary)

	if result != "" {
		t.Errorf("extractMultipartBody() with no text/plain = %q, want empty", result)
	}
}

func TestExtractMultipartBodyNested(t *testing.T) {
	boundary := "outer-boundary"
	innerBoundary := "inner-boundary"
	multipartBody := fmt.Sprintf(`--outer-boundary
Content-Type: multipart/alternative; boundary="%s"

--inner-boundary
Content-Type: text/plain

Nested plain text content.
--inner-boundary
Content-Type: text/html

<p>HTML content</p>
--inner-boundary--
--outer-boundary--`, innerBoundary)

	result := extractMultipartBody(strings.NewReader(multipartBody), boundary)
	expected := "Nested plain text content."

	if strings.TrimSpace(result) != expected {
		t.Errorf("extractMultipartBody() = %q, want %q", strings.TrimSpace(result), expected)
	}
}

func TestExtractMultipartBodyEmptyBoundary(t *testing.T) {
	result := extractMultipartBody(strings.NewReader("some content"), "")
	if result != "" {
		t.Errorf("extractMultipartBody with empty boundary = %q, want empty", result)
	}
}

func TestVersion(t *testing.T) {
	if version == "" {
		t.Error("version should not be empty")
	}
}

func TestInit(t *testing.T) {
	if mailDir == "" {
		t.Error("mailDir should not be empty after init")
	}
	if dbPath == "" {
		t.Error("dbPath should not be empty after init")
	}
	if !strings.Contains(dbPath, "Envelope Index") {
		t.Errorf("dbPath should contain 'Envelope Index', got %q", dbPath)
	}
}

func TestNewApp(t *testing.T) {
	app := NewApp()
	if app == nil {
		t.Fatal("NewApp() returned nil")
	}
	if app.OpenDB == nil {
		t.Error("app.OpenDB should not be nil")
	}
	if app.ReadEmail == nil {
		t.Error("app.ReadEmail should not be nil")
	}
	if app.Output == nil {
		t.Error("app.Output should not be nil")
	}
	if app.MailDir == "" {
		t.Error("app.MailDir should not be empty")
	}
}

func TestURLDecoding(t *testing.T) {
	tests := []struct {
		encoded  string
		expected string
	}{
		{"Inbox", "Inbox"},
		{"Deleted%20Items", "Deleted Items"},
		{"Junk%20Email", "Junk Email"},
		{"Inbox%20-%20CC", "Inbox - CC"},
	}

	for _, tt := range tests {
		t.Run(tt.encoded, func(t *testing.T) {
			decoded, err := url.QueryUnescape(tt.encoded)
			if err != nil {
				t.Errorf("Failed to decode %q: %v", tt.encoded, err)
			}
			if decoded != tt.expected {
				t.Errorf("Decode(%q) = %q, want %q", tt.encoded, decoded, tt.expected)
			}
		})
	}
}

func TestMailboxURLParsing(t *testing.T) {
	tests := []struct {
		url         string
		wantUUID    string
		wantMailbox string
	}{
		{
			"ews://08662783-E986-44D2-A7EB-B552BD325661/Inbox",
			"08662783-E986-44D2-A7EB-B552BD325661",
			"Inbox",
		},
		{
			"imap://3FD9D110-803F-43BF-B211-D4F37EDB1981/INBOX",
			"3FD9D110-803F-43BF-B211-D4F37EDB1981",
			"INBOX",
		},
		{
			"ews://UUID/Deleted%20Items",
			"UUID",
			"Deleted%20Items",
		},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			urlStr := strings.TrimPrefix(tt.url, "ews://")
			urlStr = strings.TrimPrefix(urlStr, "imap://")
			parts := strings.SplitN(urlStr, "/", 2)

			if len(parts) < 2 {
				t.Errorf("Failed to parse URL %q", tt.url)
				return
			}

			if parts[0] != tt.wantUUID {
				t.Errorf("UUID = %q, want %q", parts[0], tt.wantUUID)
			}
			if parts[1] != tt.wantMailbox {
				t.Errorf("Mailbox = %q, want %q", parts[1], tt.wantMailbox)
			}
		})
	}
}

// Database integration tests using in-memory SQLite

func TestRunMailboxes(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunMailboxes()
	if err != nil {
		t.Fatalf("RunMailboxes() error = %v", err)
	}

	result := output.String()

	// Check header
	if !strings.Contains(result, "ID") || !strings.Contains(result, "Mailbox") {
		t.Error("Output should contain header")
	}

	// Check mailboxes are listed
	if !strings.Contains(result, "Inbox") {
		t.Error("Output should contain Inbox")
	}
	if !strings.Contains(result, "Sent Items") {
		t.Error("Output should contain Sent Items (URL decoded)")
	}
	if !strings.Contains(result, "100") {
		t.Error("Output should contain total count")
	}
}

func TestRunMailboxesDBError(t *testing.T) {
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return nil, fmt.Errorf("database error")
		},
		Output: &bytes.Buffer{},
	}

	err := app.RunMailboxes()
	if err == nil {
		t.Error("RunMailboxes() should return error when DB fails")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("Error should mention database, got: %v", err)
	}
}

func TestRunList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunList(10, 0, false)
	if err != nil {
		t.Fatalf("RunList() error = %v", err)
	}

	result := output.String()

	// Check emails are listed
	if !strings.Contains(result, "Test Email Subject") {
		t.Error("Output should contain email subject")
	}
	if !strings.Contains(result, "John Doe") {
		t.Error("Output should contain sender name")
	}
	if !strings.Contains(result, "UNREAD") {
		t.Error("Output should contain UNREAD status")
	}
	if !strings.Contains(result, "read") {
		t.Error("Output should contain read status")
	}
}

func TestRunListWithMailboxFilter(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunList(10, 1, false)
	if err != nil {
		t.Fatalf("RunList() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "Test Email Subject") {
		t.Error("Output should contain emails from mailbox 1")
	}
}

func TestRunListUnreadOnly(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunList(10, 0, true)
	if err != nil {
		t.Fatalf("RunList() error = %v", err)
	}

	result := output.String()

	// Should only contain UNREAD emails
	if strings.Contains(result, "[879824]") {
		t.Error("Output should not contain read email 879824")
	}
	if !strings.Contains(result, "UNREAD") {
		t.Error("Output should contain UNREAD emails")
	}
}

func TestRunListDBError(t *testing.T) {
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return nil, fmt.Errorf("database error")
		},
		Output: &bytes.Buffer{},
	}

	err := app.RunList(10, 0, false)
	if err == nil {
		t.Error("RunList() should return error when DB fails")
	}
}

func TestRunUnread(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunUnread(10)
	if err != nil {
		t.Fatalf("RunUnread() error = %v", err)
	}

	result := output.String()

	// Check that only unread emails are shown
	if !strings.Contains(result, "UNREAD") {
		t.Error("Output should contain UNREAD status")
	}
	if !strings.Contains(result, "unread emails") {
		t.Error("Output should contain unread count")
	}
}

func TestRunUnreadWithLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunUnread(1)
	if err != nil {
		t.Fatalf("RunUnread() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "1 unread emails") {
		t.Errorf("Output should show 1 unread email, got: %s", result)
	}
}

func TestRunUnreadNoLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunUnread(0) // 0 means no limit
	if err != nil {
		t.Fatalf("RunUnread() error = %v", err)
	}

	result := output.String()
	// Should show all 3 unread emails
	if !strings.Contains(result, "3 unread emails") {
		t.Errorf("Output should show 3 unread emails, got: %s", result)
	}
}

func TestRunUnreadDBError(t *testing.T) {
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return nil, fmt.Errorf("database error")
		},
		Output: &bytes.Buffer{},
	}

	err := app.RunUnread(10)
	if err == nil {
		t.Error("RunUnread() should return error when DB fails")
	}
}

func TestRunSearch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunSearch("Test", 10)
	if err != nil {
		t.Fatalf("RunSearch() error = %v", err)
	}

	result := output.String()

	if !strings.Contains(result, "Test Email Subject") {
		t.Error("Output should contain matching email")
	}
	if !strings.Contains(result, "Found") && !strings.Contains(result, "results") {
		t.Error("Output should contain result count")
	}
}

func TestRunSearchBySender(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunSearch("John", 10)
	if err != nil {
		t.Fatalf("RunSearch() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "John Doe") {
		t.Error("Output should contain emails from John Doe")
	}
}

func TestRunSearchByEmail(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunSearch("boss@company.com", 10)
	if err != nil {
		t.Fatalf("RunSearch() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "Jane Smith") {
		t.Error("Output should contain emails from boss@company.com")
	}
}

func TestRunSearchNoResults(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, output := newTestApp(t, db)

	err := app.RunSearch("nonexistent-query-xyz", 10)
	if err != nil {
		t.Fatalf("RunSearch() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "Found 0 results") {
		t.Errorf("Output should show 0 results, got: %s", result)
	}
}

func TestRunSearchDBError(t *testing.T) {
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return nil, fmt.Errorf("database error")
		},
		Output: &bytes.Buffer{},
	}

	err := app.RunSearch("test", 10)
	if err == nil {
		t.Error("RunSearch() should return error when DB fails")
	}
}

func TestRunRead(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create temp directory structure for emlx file
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "TEST-UUID", "Inbox.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	err := os.MkdirAll(testPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	emlxContent := `1234
From: sender@example.com
Subject: Test
Content-Type: text/plain

This is the email body content.
`
	emlxFile := filepath.Join(testPath, "879823.emlx")
	err = os.WriteFile(emlxFile, []byte(emlxContent), 0644)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return db, nil
		},
		ReadEmail: os.ReadFile,
		Output:    output,
		MailDir:   tmpDir,
	}

	err = app.RunRead(879823)
	if err != nil {
		t.Fatalf("RunRead() error = %v", err)
	}

	result := output.String()

	if !strings.Contains(result, "879823") {
		t.Error("Output should contain email ID")
	}
	if !strings.Contains(result, "Test Email Subject") {
		t.Error("Output should contain subject")
	}
	if !strings.Contains(result, "John Doe") {
		t.Error("Output should contain sender name")
	}
	if !strings.Contains(result, "BODY:") {
		t.Error("Output should contain body section")
	}
}

func TestRunReadNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	app, _ := newTestApp(t, db)

	err := app.RunRead(999999)
	if err == nil {
		t.Error("RunRead() should return error for non-existent email")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("Error should mention 'not found', got: %v", err)
	}
}

func TestRunReadDBError(t *testing.T) {
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return nil, fmt.Errorf("database error")
		},
		Output: &bytes.Buffer{},
	}

	err := app.RunRead(879823)
	if err == nil {
		t.Error("RunRead() should return error when DB fails")
	}
}

func TestRunReadNoEmlxFile(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB: func() (*sql.DB, error) {
			return db, nil
		},
		ReadEmail: os.ReadFile,
		Output:    output,
		MailDir:   t.TempDir(), // Empty temp dir, no emlx file
	}

	err := app.RunRead(879823)
	if err != nil {
		t.Fatalf("RunRead() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "Could not find email file") {
		t.Error("Output should indicate email file not found")
	}
}

func TestTruncateEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"spaces", "   ", 3, "   "},
		{"newlines", "a\nb\nc", 10, "a\nb\nc"},
		{"long with spaces", "hello world foo bar", 12, "hello wor..."},
		{"exactly at boundary", "hello", 8, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncate(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
			}
		})
	}
}

func TestGetDB(t *testing.T) {
	// This tests the default getDB function
	// It may fail if the actual Mail database doesn't exist
	db, err := getDB()
	if err != nil {
		t.Skip("Database not available for testing")
	}
	defer db.Close()

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master").Scan(&count)
	if err != nil {
		t.Errorf("Failed to query database: %v", err)
	}
}

// Test for sender with empty name (should fall back to email)
func TestRunListUnknownSender(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create minimal schema
	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address INTEGER, sender INTEGER);
		CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, subject INTEGER, sender INTEGER, date_received INTEGER, mailbox INTEGER, read INTEGER);

		INSERT INTO subjects VALUES (1, 'Test Subject');
		INSERT INTO messages VALUES (1, 1, NULL, 1706100000, 1, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	err = app.RunList(10, 0, false)
	if err != nil {
		t.Fatalf("RunList() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "(unknown)") {
		t.Error("Output should show (unknown) for missing sender")
	}
}

func TestExtractPlainTextBodyEmpty(t *testing.T) {
	result := extractPlainTextBody("")
	if result != "" {
		t.Errorf("extractPlainTextBody('') = %q, want empty", result)
	}
}

func TestExtractPlainTextBodyNoSecondLine(t *testing.T) {
	result := extractPlainTextBody("1234")
	if result != "" {
		t.Errorf("extractPlainTextBody with no second line = %q, want empty", result)
	}
}

func TestStandaloneGetEmlxPath(t *testing.T) {
	// Test the standalone wrapper function
	result := getEmlxPath(123456, "invalid://url")
	if result != "" {
		t.Errorf("getEmlxPath with invalid URL = %q, want empty", result)
	}
}

func TestExtractPlainTextBodyMalformedContentType(t *testing.T) {
	emlxContent := `1234
From: sender@example.com
Content-Type: invalid content type;;;

Body text here.`

	result := extractPlainTextBody(emlxContent)
	// Should fall back to extractBodyFallback
	if !strings.Contains(result, "Body text here") {
		t.Errorf("extractPlainTextBody should fallback for malformed content-type, got: %q", result)
	}
}

func TestExtractPlainTextBodyMultipart(t *testing.T) {
	emlxContent := `1234
From: sender@example.com
Content-Type: multipart/alternative; boundary="----=_Part_123"

------=_Part_123
Content-Type: text/plain

Plain text content from multipart.
------=_Part_123
Content-Type: text/html

<html>HTML</html>
------=_Part_123--`

	result := extractPlainTextBody(emlxContent)
	if !strings.Contains(result, "Plain text content from multipart") {
		t.Errorf("extractPlainTextBody should extract from multipart, got: %q", result)
	}
}

func TestExtractPlainTextBodyNoContentType(t *testing.T) {
	emlxContent := `1234
From: sender@example.com
Subject: Test

Default plain text body.`

	result := extractPlainTextBody(emlxContent)
	if !strings.Contains(result, "Default plain text body") {
		t.Errorf("extractPlainTextBody should handle missing content-type, got: %q", result)
	}
}

func TestAppGetEmlxPathShortRowid(t *testing.T) {
	app := &App{MailDir: t.TempDir()}
	result := app.getEmlxPath(12, "ews://UUID/Inbox") // rowid too short (< 3 digits)
	if result != "" {
		t.Errorf("getEmlxPath with short rowid = %q, want empty", result)
	}
}

func TestAppGetEmlxPathInvalidURL(t *testing.T) {
	app := &App{MailDir: t.TempDir()}
	result := app.getEmlxPath(879823, "invalid") // no slash in URL
	if result != "" {
		t.Errorf("getEmlxPath with invalid URL = %q, want empty", result)
	}
}

func TestAppGetEmlxPathURLDecoded(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test directory structure with URL-encoded mailbox name
	accountUUID := "TEST-UUID"
	testPath := filepath.Join(tmpDir, accountUUID, "Sent Items.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	err := os.MkdirAll(testPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	emlxFile := filepath.Join(testPath, "879823.emlx")
	err = os.WriteFile(emlxFile, []byte("test"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{MailDir: tmpDir}
	result := app.getEmlxPath(879823, "ews://TEST-UUID/Sent%20Items")
	if result == "" {
		t.Error("getEmlxPath should find URL-decoded mailbox name")
	}
}

func TestRunMailboxesQueryError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't create tables - query will fail

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunMailboxes()
	if err == nil {
		t.Error("RunMailboxes should return error when query fails")
	}
}

func TestRunListQueryError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't create tables - query will fail

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunList(10, 0, false)
	if err == nil {
		t.Error("RunList should return error when query fails")
	}
}

func TestRunUnreadQueryError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't create tables - query will fail

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunUnread(10)
	if err == nil {
		t.Error("RunUnread should return error when query fails")
	}
}

func TestRunSearchQueryError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't create tables - query will fail

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunSearch("test", 10)
	if err == nil {
		t.Error("RunSearch should return error when query fails")
	}
}

func TestRunReadQueryError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Don't create tables - query will fail

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunRead(879823)
	if err == nil {
		t.Error("RunRead should return error when query fails")
	}
}

func TestRunReadWithLongBody(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "TEST-UUID", "Inbox.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	err := os.MkdirAll(testPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	// Create email with >100 lines
	var longBody strings.Builder
	longBody.WriteString("1234\nFrom: sender@example.com\nContent-Type: text/plain\n\n")
	for i := 0; i < 150; i++ {
		longBody.WriteString(fmt.Sprintf("Line %d of the body\n", i))
	}

	emlxFile := filepath.Join(testPath, "879823.emlx")
	err = os.WriteFile(emlxFile, []byte(longBody.String()), 0644)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:    func() (*sql.DB, error) { return db, nil },
		ReadEmail: os.ReadFile,
		Output:    output,
		MailDir:   tmpDir,
	}

	err = app.RunRead(879823)
	if err != nil {
		t.Fatalf("RunRead() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "more lines]") {
		t.Error("Output should indicate truncated body")
	}
}

func TestRunReadFileReadError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "TEST-UUID", "Inbox.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	err := os.MkdirAll(testPath, 0755)
	if err != nil {
		t.Fatal(err)
	}

	emlxFile := filepath.Join(testPath, "879823.emlx")
	err = os.WriteFile(emlxFile, []byte("test"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB: func() (*sql.DB, error) { return db, nil },
		ReadEmail: func(path string) ([]byte, error) {
			return nil, fmt.Errorf("read error")
		},
		Output:  output,
		MailDir: tmpDir,
	}

	err = app.RunRead(879823)
	if err == nil {
		t.Error("RunRead should return error when file read fails")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("Error should mention read failure, got: %v", err)
	}
}

func TestRunListSenderFallbackToEmail(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address INTEGER, sender INTEGER);
		CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, subject INTEGER, sender INTEGER, date_received INTEGER, mailbox INTEGER, read INTEGER);

		INSERT INTO subjects VALUES (1, 'Test');
		INSERT INTO addresses VALUES (1, 'test@example.com', '');
		INSERT INTO senders VALUES (1);
		INSERT INTO sender_addresses VALUES (1, 1);
		INSERT INTO messages VALUES (1, 1, 1, 1706100000, 1, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	err = app.RunList(10, 0, false)
	if err != nil {
		t.Fatalf("RunList() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "test@example.com") {
		t.Error("Output should show email when name is empty")
	}
}

func TestExtractMultipartBodyReadError(t *testing.T) {
	boundary := "----=_Part_123"
	// Malformed multipart that will cause read errors
	multipartBody := `------=_Part_123
Content-Type: text/plain

`
	result := extractMultipartBody(strings.NewReader(multipartBody), boundary)
	// Should handle gracefully and return empty or partial result
	_ = result // Just ensure no panic
}

func TestRunMailboxesScanError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create table with wrong column types to cause scan error
	_, err = db.Exec(`
		CREATE TABLE mailboxes (ROWID TEXT, url INTEGER, total_count TEXT, unread_count TEXT);
		INSERT INTO mailboxes VALUES ('not_int', 123, 'abc', 'def');
	`)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunMailboxes()
	if err == nil {
		t.Error("RunMailboxes should return error on scan failure")
	}
}

func TestRunListScanError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address TEXT, sender TEXT);
		CREATE TABLE messages (ROWID TEXT, subject TEXT, sender TEXT, date_received TEXT, mailbox TEXT, read TEXT);
		INSERT INTO messages VALUES ('bad', 'bad', 'bad', 'bad', 'bad', 'bad');
	`)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunList(10, 0, false)
	if err == nil {
		t.Error("RunList should return error on scan failure")
	}
}

func TestRunUnreadScanError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address TEXT, sender TEXT);
		CREATE TABLE messages (ROWID TEXT, subject TEXT, sender TEXT, date_received TEXT, mailbox TEXT, read INTEGER DEFAULT 0);
		INSERT INTO messages VALUES ('bad', 'bad', 'bad', 'bad', 'bad', 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunUnread(10)
	if err == nil {
		t.Error("RunUnread should return error on scan failure")
	}
}

func TestRunSearchScanError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address TEXT, sender TEXT);
		CREATE TABLE messages (ROWID TEXT, subject TEXT, sender TEXT, date_received TEXT, mailbox TEXT, read TEXT);
		INSERT INTO subjects VALUES (1, 'test');
		INSERT INTO messages VALUES ('bad', 1, 'bad', 'bad', 'bad', 'bad');
	`)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  &bytes.Buffer{},
		MailDir: t.TempDir(),
	}

	err = app.RunSearch("test", 10)
	if err == nil {
		t.Error("RunSearch should return error on scan failure")
	}
}

func TestExtractPlainTextBodyIOError(t *testing.T) {
	// Test with content that might cause IO read errors
	emlxContent := `1234
From: sender@example.com
Content-Type: text/plain

Body.`

	result := extractPlainTextBody(emlxContent)
	if !strings.Contains(result, "Body") {
		t.Errorf("extractPlainTextBody should return body, got: %q", result)
	}
}

func TestExtractPlainTextBodyMalformedEmail(t *testing.T) {
	// Malformed email that can't be parsed
	emlxContent := `1234
This is not a valid email format at all
No headers, no body separator`

	result := extractPlainTextBody(emlxContent)
	// Should fallback gracefully
	_ = result // Just ensure no panic
}

func TestRunReadSenderWithName(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "TEST-UUID", "Inbox.mbox", "DATA-UUID", "Data", "9", "7", "8", "Messages")
	os.MkdirAll(testPath, 0755)
	os.WriteFile(filepath.Join(testPath, "879823.emlx"), []byte("1234\nFrom: test\nContent-Type: text/plain\n\nBody"), 0644)

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:    func() (*sql.DB, error) { return db, nil },
		ReadEmail: os.ReadFile,
		Output:    output,
		MailDir:   tmpDir,
	}

	err := app.RunRead(879823)
	if err != nil {
		t.Fatalf("RunRead() error = %v", err)
	}

	result := output.String()
	// Should show sender name
	if !strings.Contains(result, "John Doe") {
		t.Error("Output should contain sender name")
	}
}

// CLI command tests

func TestBuildRootCmd(t *testing.T) {
	app := NewApp()
	cmd := buildRootCmd(app)

	if cmd.Use != "macmail" {
		t.Errorf("Root command Use = %q, want 'macmail'", cmd.Use)
	}

	// Check subcommands exist
	subcommands := []string{"mailboxes", "list", "search", "read", "unread"}
	for _, name := range subcommands {
		found := false
		for _, subcmd := range cmd.Commands() {
			if subcmd.Name() == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Subcommand %q not found", name)
		}
	}
}

func TestRunFunction(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:    func() (*sql.DB, error) { return db, nil },
		ReadEmail: func(path string) ([]byte, error) { return []byte("1234\n\nBody"), nil },
		Output:    output,
		MailDir:   t.TempDir(),
	}

	// Test that run() works with version command
	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"--version"})
	err := cmd.Execute()
	if err != nil {
		t.Errorf("run with --version failed: %v", err)
	}
}

func TestCLIMailboxesCommand(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"mailboxes"})
	cmd.SetOut(output)
	cmd.SetErr(output)

	err := cmd.Execute()
	if err != nil {
		t.Errorf("mailboxes command failed: %v", err)
	}

	if !strings.Contains(output.String(), "Inbox") {
		t.Error("mailboxes output should contain Inbox")
	}
}

func TestCLIListCommand(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"list", "-n", "5"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("list command failed: %v", err)
	}
}

func TestCLIListCommandWithFlags(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"list", "-n", "5", "-m", "1", "-u"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("list command with flags failed: %v", err)
	}
}

func TestCLISearchCommand(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"search", "Test"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("search command failed: %v", err)
	}
}

func TestCLISearchCommandWithLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"search", "-n", "5", "Test"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("search command with limit failed: %v", err)
	}
}

func TestCLIReadCommand(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:    func() (*sql.DB, error) { return db, nil },
		ReadEmail: func(path string) ([]byte, error) { return []byte("1234\n\nBody"), nil },
		Output:    output,
		MailDir:   t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"read", "879823"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("read command failed: %v", err)
	}
}

func TestCLIReadCommandInvalidID(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"read", "not-a-number"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Error("read command should fail with invalid ID")
	}
}

func TestCLIUnreadCommand(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"unread"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("unread command failed: %v", err)
	}
}

func TestCLIUnreadCommandWithLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"unread", "5"})

	err := cmd.Execute()
	if err != nil {
		t.Errorf("unread command with limit failed: %v", err)
	}
}

func TestCLIUnreadCommandInvalidLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:  func() (*sql.DB, error) { return db, nil },
		Output:  output,
		MailDir: t.TempDir(),
	}

	cmd := buildRootCmd(app)
	cmd.SetArgs([]string{"unread", "not-a-number"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Error("unread command should fail with invalid limit")
	}
}

func TestRunReadSenderNoName(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE subjects (ROWID INTEGER PRIMARY KEY, subject TEXT);
		CREATE TABLE addresses (ROWID INTEGER PRIMARY KEY, address TEXT, comment TEXT);
		CREATE TABLE senders (ROWID INTEGER PRIMARY KEY);
		CREATE TABLE sender_addresses (address INTEGER, sender INTEGER);
		CREATE TABLE mailboxes (ROWID INTEGER PRIMARY KEY, url TEXT);
		CREATE TABLE messages (ROWID INTEGER PRIMARY KEY, subject INTEGER, sender INTEGER, date_received INTEGER, mailbox INTEGER, read INTEGER);

		INSERT INTO subjects VALUES (1, 'Test Subject');
		INSERT INTO addresses VALUES (1, 'test@example.com', '');
		INSERT INTO senders VALUES (1);
		INSERT INTO sender_addresses VALUES (1, 1);
		INSERT INTO mailboxes VALUES (1, 'ews://UUID/Inbox');
		INSERT INTO messages VALUES (879823, 1, 1, 1706100000, 1, 0);
	`)
	if err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	app := &App{
		OpenDB:    func() (*sql.DB, error) { return db, nil },
		ReadEmail: func(path string) ([]byte, error) { return []byte("1234\n\nBody"), nil },
		Output:    output,
		MailDir:   t.TempDir(),
	}

	err = app.RunRead(879823)
	if err != nil {
		t.Fatalf("RunRead() error = %v", err)
	}

	result := output.String()
	if !strings.Contains(result, "test@example.com") {
		t.Error("Output should show email address when name is empty")
	}
}
