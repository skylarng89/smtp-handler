package transport

import (
	"bytes"
	"fmt"
	netmail "net/mail"

	mail "github.com/wneessen/go-mail"

	"github.com/skylarng89/smtp-handler/internal/message"
)

// BuildMsg converts a canonical message into a go-mail Msg. The Message-ID
// and Date are taken from the stored message so every retry sends identical
// headers (receivers can then de-duplicate a resend).
func BuildMsg(m *message.Message) (*mail.Msg, error) {
	msg := mail.NewMsg(mail.WithNoDefaultUserAgent())

	msg.FromMailAddress(m.From.Net())
	msg.ToMailAddress(addrs(m.To)...)
	msg.CcMailAddress(addrs(m.CC)...)
	msg.BccMailAddress(addrs(m.BCC)...)
	if len(m.ReplyTo) > 0 {
		msg.ReplyToMailAddress(addrs(m.ReplyTo)...)
	}
	msg.Subject(m.Subject)
	msg.SetMessageIDWithValue(m.MessageID)
	msg.SetDateWithValue(m.Date)

	switch {
	case m.Text != "" && m.HTML != "":
		msg.SetBodyString(mail.TypeTextPlain, m.Text)
		msg.AddAlternativeString(mail.TypeTextHTML, m.HTML)
	case m.HTML != "":
		msg.SetBodyString(mail.TypeTextHTML, m.HTML)
	case m.Text != "":
		msg.SetBodyString(mail.TypeTextPlain, m.Text)
	default:
		return nil, fmt.Errorf("message %s has no body", m.ID)
	}

	for _, a := range m.Attach {
		if err := msg.AttachReader(a.Filename, bytes.NewReader(a.Data),
			mail.WithFileContentType(mail.ContentType(a.ContentType))); err != nil {
			return nil, fmt.Errorf("attach %q: %w", a.Filename, err)
		}
	}
	return msg, nil
}

func addrs(in []message.Address) []*netmail.Address {
	out := make([]*netmail.Address, len(in))
	for i, a := range in {
		out[i] = a.Net()
	}
	return out
}
