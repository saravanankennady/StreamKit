package routes

import (
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"elnova-streamkit.local/server/config"
)

// NewMailer picks how to send mail: SMTP when configured, files in a directory for development and tests,
// otherwise a warning in the log, without the message, whose links are secrets.
func NewMailer(cfg config.Mail, log *slog.Logger) Mailer {
	switch {
	case cfg.SMTPHost != "":
		return &smtpMailer{cfg: cfg}
	case cfg.Dir != "":
		return &fileMailer{dir: cfg.Dir}
	}
	return &noMailer{log: log}
}

type smtpMailer struct{ cfg config.Mail }

func (m *smtpMailer) Send(to, subject, body string) error {
	port := m.cfg.SMTPPort
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(m.cfg.SMTPHost, strconv.Itoa(port))
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, m.cfg.From, []string{to}, []byte(formatMessage(m.cfg.From, to, subject, body)))
}

func formatMessage(from, to, subject, body string) string {
	clean := strings.NewReplacer("\r", "", "\n", "")
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		clean.Replace(from), clean.Replace(to), clean.Replace(subject), body)
}

type fileMailer struct{ dir string }

func (m *fileMailer) Send(to, subject, body string) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s.eml", time.Now().UnixNano(), strings.NewReplacer("/", "_", "@", "_at_").Replace(to))
	return os.WriteFile(filepath.Join(m.dir, name), []byte(formatMessage("noreply@localhost", to, subject, body)), 0o600)
}

type noMailer struct{ log *slog.Logger }

func (m *noMailer) Send(to, subject, _ string) error {
	m.log.Warn("no mail transport is configured, so an email was not sent", "subject", subject)
	return nil
}
