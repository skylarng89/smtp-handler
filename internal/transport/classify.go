package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"

	mail "github.com/wneessen/go-mail"
)

const maxDetailLen = 500

// authCodes are SMTP replies that mean "we are not allowed", not "this
// message is bad": authentication required/failed, TLS required, etc.
var authCodes = map[int]bool{530: true, 534: true, 535: true, 538: true}

var codeRe = regexp.MustCompile(`(?:^|[^0-9])([45][0-9]{2})[ -]`)

func detail(err error) string {
	s := err.Error()
	if len(s) > maxDetailLen {
		s = s[:maxDetailLen] + "…"
	}
	return s
}

// smtpCode extracts an SMTP reply code from an error, preferring the typed
// textproto.Error and falling back to the leading code in the text.
func smtpCode(err error) int {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return tpErr.Code
	}
	if m := codeRe.FindStringSubmatch(err.Error()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// ClassifyDial classifies an error from connecting, STARTTLS or
// authentication. Nothing has been sent yet, so every outcome is safe to
// retry; the question is only whether retrying is useful.
func ClassifyDial(err error) Result {
	res := Result{Detail: detail(err)}
	if code := smtpCode(err); code != 0 {
		res.Code = code
	}

	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		certInvalid      x509.CertificateInvalidError
		verifyErr        *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname),
		errors.As(err, &certInvalid), errors.As(err, &verifyErr):
		res.Class = ClassConfig
	case res.Code >= 500:
		// The server refused the session itself (greeting, EHLO, auth).
		res.Class = ClassConfig
	case strings.Contains(err.Error(), "STARTTLS"):
		// TLS is mandatory but the server cannot provide it.
		res.Class = ClassConfig
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		res.Class = ClassTransient
	default:
		res.Class = ClassTransient
	}
	return res
}

// ClassifySend classifies an error from the MAIL/RCPT/DATA exchange.
func ClassifySend(err error) Result {
	if err == nil {
		return Result{Class: ClassSent}
	}
	res := Result{Detail: detail(err)}

	var sendErr *mail.SendError
	if errors.As(err, &sendErr) {
		res.Code = sendErr.ErrorCode()
		res.Enhanced = sendErr.EnhancedStatusCode()
		switch {
		case res.Code >= 500 && authCodes[res.Code]:
			res.Class = ClassConfig
		case res.Code >= 500:
			res.Class = ClassPermanent
		case res.Code >= 400:
			res.Class = ClassTransient
		case sendErr.Reason == mail.ErrSMTPDataClose:
			// The body was fully sent but the server's verdict never
			// arrived: it may have accepted the message.
			res.Class = ClassUnknown
		default:
			// Failed before the server could have accepted the message.
			res.Class = ClassTransient
		}
		return res
	}

	if code := smtpCode(err); code != 0 {
		res.Code = code
	}
	switch {
	case res.Code >= 500 && authCodes[res.Code]:
		res.Class = ClassConfig
	case res.Code >= 500:
		res.Class = ClassPermanent
	default:
		res.Class = ClassTransient
	}
	return res
}
