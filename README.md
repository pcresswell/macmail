# macmail

A command-line tool to query and read emails stored locally by Apple Mail on macOS.

macmail reads directly from Mail's SQLite database and `.emlx` files — no IMAP connections, no network access, no Apple Mail APIs. Everything stays local.

## Prerequisites

- macOS with Apple Mail configured
- Go 1.21+
- **Full Disk Access** must be granted to your terminal app (System Settings > Privacy & Security > Full Disk Access) so macmail can read `~/Library/Mail/`

## Install

```bash
go install github.com/pcresswell/macmail/cmd/macmail@latest
```

Or build from source:

```bash
git clone https://github.com/pcresswell/macmail.git
cd macmail
make install
```

## Usage

### List mailboxes

```bash
macmail mailboxes
```

### List recent emails

```bash
macmail list              # last 50 emails
macmail list -n 10        # last 10
macmail list -m 3         # from mailbox ID 3
macmail list -u           # unread only
```

### Show unread emails

```bash
macmail unread            # up to 50 unread
macmail unread 10         # up to 10 unread
```

### Search by subject or sender

```bash
macmail search "invoice"
macmail search "john@example.com" -n 20
```

### Read an email

```bash
macmail read 12345
```

### List and save attachments

```bash
macmail attachments 12345              # list attachments
macmail attachments 12345 -s ./saved   # save to directory
```

## How it works

macmail queries Apple Mail's Envelope Index SQLite database at `~/Library/Mail/V10/MailData/Envelope Index` for message metadata (sender, subject, date, read status). When reading a full email, it locates the corresponding `.emlx` file on disk and parses the MIME content to extract the plain text body or attachments.

## License

[MIT](LICENSE)
