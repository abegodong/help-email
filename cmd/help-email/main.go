package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	netmail "net/mail"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message"
	msgmail "github.com/emersion/go-message/mail"
)

const defaultPollInterval = 30 * time.Second
const defaultAPIMaxRetries = 5
const defaultAPIRetryBackoff = 2 * time.Second
const defaultHTTPTimeout = 30 * time.Second

type Config struct {
	IMAPHost     string
	Username     string
	Password     string
	Mailbox      string
	APIEndpoint  string
	PollInterval time.Duration
	StateFile    string
	APIMaxRetries  int
	APIRetryBackoff time.Duration
	HTTPTimeout    time.Duration
	ProcessExisting bool
}

type State struct {
	LastUID     uint32 `json:"last_uid"`
	Initialized bool   `json:"initialized"`
}

type EmailPayload struct {
	MessageID  string        `json:"message_id,omitempty"`
	Subject    string        `json:"subject,omitempty"`
	SentAt     *time.Time    `json:"sent_at,omitempty"`
	ReceivedAt time.Time     `json:"received_at"`
	Sender     Party         `json:"sender"`
	Forwarder  *Party        `json:"forwarder,omitempty"`
	Recipients []Recipient   `json:"recipients"`
	Body       Body          `json:"body"`
	Attachments []Attachment `json:"attachments,omitempty"`
	IMAP       IMAPMeta      `json:"imap"`
}

type Party struct {
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}

type Recipient struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Type  string `json:"type"`
}

type Body struct {
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`
}

type Attachment struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	ContentBase64 string `json:"content_base64"`
}

type IMAPMeta struct {
	Mailbox string `json:"mailbox"`
	UID     uint32 `json:"uid"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg Config) error {
	state, err := loadState(cfg.StateFile)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := pollOnce(ctx, cfg, &state); err != nil {
			log.Printf("poll failed: %v", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func pollOnce(ctx context.Context, cfg Config, state *State) error {
	c, err := client.DialTLS(cfg.IMAPHost, &tls.Config{ServerName: hostOnly(cfg.IMAPHost)})
	if err != nil {
		return fmt.Errorf("connect imap: %w", err)
	}
	defer c.Logout()

	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		return fmt.Errorf("login imap: %w", err)
	}

	mbox, err := c.Select(cfg.Mailbox, false)
	if err != nil {
		return fmt.Errorf("select mailbox: %w", err)
	}

	if !state.Initialized {
		state.Initialized = true
		if !cfg.ProcessExisting {
			state.LastUID = lastExistingUID(mbox)
		}
		if err := saveState(cfg.StateFile, *state); err != nil {
			return err
		}
		if !cfg.ProcessExisting {
			return nil
		}
	}

	if mbox.Messages == 0 {
		return nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddRange(state.LastUID+1, 0)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, section.FetchItem()}
	messages := make(chan *imap.Message, 20)
	done := make(chan error, 1)

	go func() {
		done <- c.UidFetch(seqset, items, messages)
	}()

	var postedUIDs []uint32
	var processingErr error
	for msg := range messages {
		if processingErr != nil {
			continue
		}
		if msg == nil || msg.Uid <= state.LastUID {
			continue
		}

		body := msg.GetBody(section)
		if body == nil {
			processingErr = fmt.Errorf("uid %d had no body", msg.Uid)
			continue
		}

		payload, err := parseMessage(body, cfg.Mailbox, msg.Uid)
		if err != nil {
			processingErr = fmt.Errorf("parse uid %d: %w", msg.Uid, err)
			continue
		}

		if err := postPayloadWithRetry(ctx, cfg, payload); err != nil {
			processingErr = fmt.Errorf("post uid %d: %w", msg.Uid, err)
			continue
		}

		postedUIDs = append(postedUIDs, msg.Uid)
	}

	if err := <-done; err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}

	maxReadUID := state.LastUID
	for _, uid := range postedUIDs {
		if err := markRead(c, uid); err != nil {
			if maxReadUID != state.LastUID {
				state.LastUID = maxReadUID
				if err := saveState(cfg.StateFile, *state); err != nil {
					return err
				}
			}
			return fmt.Errorf("mark uid %d read: %w", uid, err)
		}

		if uid > maxReadUID {
			maxReadUID = uid
		}
	}

	if processingErr != nil {
		if maxReadUID != state.LastUID {
			state.LastUID = maxReadUID
			if err := saveState(cfg.StateFile, *state); err != nil {
				return err
			}
		}
		return processingErr
	}

	if maxReadUID != state.LastUID {
		state.LastUID = maxReadUID
		if err := saveState(cfg.StateFile, *state); err != nil {
			return err
		}
	}

	return nil
}

func lastExistingUID(mbox *imap.MailboxStatus) uint32 {
	if mbox.UidNext == 0 {
		return 0
	}
	return mbox.UidNext - 1
}

func parseMessage(r io.Reader, mailbox string, uid uint32) (EmailPayload, error) {
	entity, err := message.Read(r)
	if err != nil {
		return EmailPayload{}, err
	}

	header := msgmail.Header{Header: entity.Header}
	from := firstAddress(addressList(header, "From"))
	recipients := collectRecipients(header)

	subject, _ := header.Subject()
	date, _ := header.Date()
	messageID := header.Get("Message-Id")

	body, attachments, err := readEntity(entity)
	if err != nil {
		return EmailPayload{}, err
	}

	sender := Party{Email: from.Address, Name: from.Name, Source: "header"}
	var forwarder *Party
	if original := detectForwardedSender(body.Text + "\n" + body.HTML); original.Email != "" {
		forwarder = &Party{Email: from.Address, Name: from.Name}
		sender = original
		sender.Source = "forwarded_body"
	}

	var sentAt *time.Time
	if !date.IsZero() {
		sentAt = &date
	}

	return EmailPayload{
		MessageID:   messageID,
		Subject:     subject,
		SentAt:      sentAt,
		ReceivedAt:  time.Now().UTC(),
		Sender:      sender,
		Forwarder:   forwarder,
		Recipients:  recipients,
		Body:        body,
		Attachments: attachments,
		IMAP:        IMAPMeta{Mailbox: mailbox, UID: uid},
	}, nil
}

func readEntity(entity *message.Entity) (Body, []Attachment, error) {
	var body Body
	var attachments []Attachment

	mr := msgmail.NewReader(entity)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return body, attachments, err
		}

		switch h := part.Header.(type) {
		case *msgmail.InlineHeader:
			contentType, _, _ := h.ContentType()
			b, err := io.ReadAll(part.Body)
			if err != nil {
				return body, attachments, err
			}
			switch strings.ToLower(contentType) {
			case "text/plain":
				body.Text += string(b)
			case "text/html":
				body.HTML += string(b)
			}
		case *msgmail.AttachmentHeader:
			att, err := readAttachment(h, part.Body)
			if err != nil {
				return body, attachments, err
			}
			attachments = append(attachments, att)
		}
	}

	return body, attachments, nil
}

func readAttachment(header *msgmail.AttachmentHeader, r io.Reader) (Attachment, error) {
	filename, _ := header.Filename()
	contentType, _, _ := header.ContentType()
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	var buf bytes.Buffer
	hash := sha256.New()
	encoded := base64.NewEncoder(base64.StdEncoding, &buf)
	size, err := io.Copy(io.MultiWriter(encoded, hash), r)
	if closeErr := encoded.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Attachment{}, err
	}

	return Attachment{
		Filename:      filename,
		ContentType:   contentType,
		Size:          size,
		SHA256:        hex.EncodeToString(hash.Sum(nil)),
		ContentBase64: buf.String(),
	}, nil
}

func detectForwardedSender(text string) Party {
	candidates := []*regexp.Regexp{
		regexp.MustCompile(`(?im)^From:\s*(.+<[^>]+>|[^\s<]+@[^\s>]+)`),
		regexp.MustCompile(`(?im)^De:\s*(.+<[^>]+>|[^\s<]+@[^\s>]+)`),
		regexp.MustCompile(`(?im)^Original From:\s*(.+<[^>]+>|[^\s<]+@[^\s>]+)`),
		regexp.MustCompile(`(?im)^Begin forwarded message:\s*\n(?:.*\n){0,8}?From:\s*(.+<[^>]+>|[^\s<]+@[^\s>]+)`),
	}

	for _, re := range candidates {
		match := re.FindStringSubmatch(text)
		if len(match) < 2 {
			continue
		}
		if party := parseParty(match[1]); party.Email != "" {
			return party
		}
	}

	return Party{}
}

func parseParty(raw string) Party {
	raw = strings.TrimSpace(raw)
	if addr, err := netmail.ParseAddress(raw); err == nil {
		return Party{Email: addr.Address, Name: addr.Name}
	}

	emailRe := regexp.MustCompile(`[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}`)
	email := emailRe.FindString(strings.ToUpper(raw))
	if email == "" {
		return Party{}
	}
	return Party{Email: strings.ToLower(email)}
}

func collectRecipients(header msgmail.Header) []Recipient {
	var recipients []Recipient
	for _, kind := range []struct {
		header string
		typ    string
	}{
		{"To", "to"},
		{"Cc", "cc"},
		{"Bcc", "bcc"},
	} {
		for _, addr := range addressList(header, kind.header) {
			recipients = append(recipients, Recipient{
				Email: addr.Address,
				Name:  addr.Name,
				Type:  kind.typ,
			})
		}
	}
	return recipients
}

func addressList(header msgmail.Header, key string) []*msgmail.Address {
	addresses, err := header.AddressList(key)
	if err != nil {
		return nil
	}
	return addresses
}

func firstAddress(addresses []*msgmail.Address) *msgmail.Address {
	if len(addresses) == 0 {
		return &msgmail.Address{}
	}
	return addresses[0]
}

func markRead(c *client.Client, uid uint32) error {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.SeenFlag}
	return c.UidStore(seqset, item, flags, nil)
}

func postPayloadWithRetry(ctx context.Context, cfg Config, payload EmailPayload) error {
	var lastErr error

	for attempt := 1; attempt <= cfg.APIMaxRetries; attempt++ {
		if err := postPayload(ctx, cfg, payload); err != nil {
			lastErr = err
			if attempt == cfg.APIMaxRetries {
				break
			}

			wait := cfg.APIRetryBackoff * time.Duration(attempt)
			log.Printf("api submission failed for uid %d, retrying in %s: %v", payload.IMAP.UID, wait, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		return nil
	}

	return fmt.Errorf("api submission failed after %d attempts: %w", cfg.APIMaxRetries, lastErr)
}

func postPayload(ctx context.Context, cfg Config, payload EmailPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.APIEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey(payload))

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("api returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	return nil
}

func idempotencyKey(payload EmailPayload) string {
	if payload.MessageID != "" {
		return payload.MessageID
	}
	return fmt.Sprintf("%s:%d", payload.IMAP.Mailbox, payload.IMAP.UID)
}

func loadConfig() (Config, error) {
	interval := defaultPollInterval
	if raw := os.Getenv("POLL_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
		}
		interval = parsed
	}

	apiRetryBackoff := defaultAPIRetryBackoff
	if raw := os.Getenv("API_RETRY_BACKOFF"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid API_RETRY_BACKOFF: %w", err)
		}
		apiRetryBackoff = parsed
	}

	httpTimeout := defaultHTTPTimeout
	if raw := os.Getenv("HTTP_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid HTTP_TIMEOUT: %w", err)
		}
		httpTimeout = parsed
	}

	apiMaxRetries := defaultAPIMaxRetries
	if raw := os.Getenv("API_MAX_RETRIES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return Config{}, fmt.Errorf("API_MAX_RETRIES must be a positive integer")
		}
		apiMaxRetries = parsed
	}

	processExisting := false
	if raw := os.Getenv("PROCESS_EXISTING"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("PROCESS_EXISTING must be true or false")
		}
		processExisting = parsed
	}

	cfg := Config{
		IMAPHost:     envDefault("IMAP_HOST", "imap.gmail.com:993"),
		Username:     os.Getenv("IMAP_USERNAME"),
		Password:     os.Getenv("IMAP_PASSWORD"),
		Mailbox:      envDefault("IMAP_MAILBOX", "INBOX"),
		APIEndpoint:  os.Getenv("API_ENDPOINT"),
		PollInterval: interval,
		StateFile:    envDefault("STATE_FILE", ".help-email-state.json"),
		APIMaxRetries:  apiMaxRetries,
		APIRetryBackoff: apiRetryBackoff,
		HTTPTimeout:    httpTimeout,
		ProcessExisting: processExisting,
	}

	var missing []string
	for key, value := range map[string]string{
		"IMAP_USERNAME": cfg.Username,
		"IMAP_PASSWORD": cfg.Password,
		"API_ENDPOINT":  cfg.APIEndpoint,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func loadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}

	var state State
	if err := json.Unmarshal(b, &state); err != nil {
		return State{}, err
	}
	if state.LastUID > 0 {
		state.Initialized = true
	}
	return state, nil
}

func saveState(path string, state State) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func hostOnly(addr string) string {
	host := addr
	if strings.Contains(addr, ":") {
		host = strings.Split(addr, ":")[0]
	}
	return host
}
