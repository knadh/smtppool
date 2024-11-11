// Package smtppool creates a pool of reusable SMTP connections for high
// throughput e-mailing.
//
// This file was forked from:
// https://github.com/jordan-wright/email (MIT License, Copyright (c) 2013 Jordan Wright).
package smtppool

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Global constants.
const (
	ContentTypePlain            = "text/plain"
	ContentTypeHTML             = "text/html"
	ContentTypeOctetStream      = "application/octet-stream"
	ContentTypeMultipartAlt     = "multipart/alternative"
	ContentTypeMultipartMixed   = "multipart/mixed"
	ContentTypeMultipartRelated = "multipart/related"

	// MaxLineLength is the maximum line length per RFC 2045.
	MaxLineLength = 76
)

// SMTP headers and fields.
const (
	HdrContentType = "Content-Type"
	HdrSubject     = "Subject"
	HdrTo          = "To"
	HdrCC          = "Cc"
	HdrBCC         = "Bcc"
	HdrFrom        = "From"
	HdrReplyTo     = "Reply-To"
	HdrDate        = "Date"
	HdrMessageID   = "Message-Id"
	HdrMimeVersion = "MIME-Version"

	HdrContentTransferEncoding = "Content-Transfer-Encoding"
	HdrContentDisposition      = "Content-Disposition"
	HdrContentID               = "Content-ID"
)

const (
	// defaultContentType is the default Content-Type according to RFC 2045, section 5.2.
	defaultContentType  = "text/plain; charset=us-ascii"
	defaultCharEncoding = "UTF-8"
	defaultMimeVersion  = "1.0"
	defaultHostname     = "localhost.localdomain"

	contentEncBase64          = "base64"
	contentEncQuotedPrintable = "quoted-printable"
	paramBoundary             = "boundary"
)

var (
	// ErrMissingBoundary is returned when there is no boundary given for a multipart entity.
	ErrMissingBoundary = errors.New("No boundary found for multipart entity")

	// ErrMissingContentType is returned when there is no "Content-Type" header for a MIME entity.
	ErrMissingContentType = errors.New("No Content-Type found for MIME entity")

	msgHeaders = []string{HdrReplyTo, HdrTo, HdrCC, HdrFrom, HdrSubject,
		HdrDate, HdrMessageID, HdrMimeVersion}

	maxBigInt = big.NewInt(math.MaxInt64)
)

// Email represents an e-mail message.
type Email struct {
	ReplyTo []string
	From    string
	To      []string
	Bcc     []string
	Cc      []string
	Subject string

	// Text is the optional plain text form of the message.
	Text []byte

	// HTML is the optional HTML form of the message.
	HTML []byte

	// Sender overrides From as SMTP envelope sender (optional).
	Sender      string
	Headers     textproto.MIMEHeader
	Attachments []Attachment
	ReadReceipt []string
}

// Attachment is a struct representing an email attachment.
// Based on the mime/multipart.FileHeader struct, Attachment contains the name,
// MIMEHeader, and content of the attachment in question.
type Attachment struct {
	Filename    string
	Header      textproto.MIMEHeader
	Content     []byte
	HTMLRelated bool
}

// part is a copyable representation of a multipart.Part.
type part struct {
	header textproto.MIMEHeader
	body   []byte
}

// trimReader is a custom io.Reader that will trim any leading
// whitespace, as this can cause email imports to fail.
type trimReader struct {
	rd io.Reader
}

// Read trims off any unicode whitespace from the originating reader.
func (tr trimReader) Read(buf []byte) (int, error) {
	var (
		n, err = tr.rd.Read(buf)
		t      = bytes.TrimLeftFunc(buf[:n], unicode.IsSpace)
	)
	n = copy(buf, t)
	return n, err
}

// NewEmailFromReader reads a stream of bytes from an io.Reader, r,
// and returns an email struct containing the parsed data.
// This function expects the data in RFC 5322 format.
func NewEmailFromReader(r io.Reader) (Email, error) {
	var (
		e  = Email{Headers: textproto.MIMEHeader{}}
		s  = trimReader{rd: r}
		tp = textproto.NewReader(bufio.NewReader(s))
	)

	// Parse the main headers.
	hdrs, err := tp.ReadMIMEHeader()
	if err != nil {
		return e, err
	}
	// Set the subject, to, cc, bcc, and from.
	for h, v := range hdrs {
		switch {
		case h == HdrSubject:
			e.Subject = v[0]
			subj, err := (&mime.WordDecoder{}).DecodeHeader(e.Subject)
			if err == nil && len(subj) > 0 {
				e.Subject = subj
			}
			delete(hdrs, h)
		case h == HdrTo:
			for _, to := range v {
				tt, err := (&mime.WordDecoder{}).DecodeHeader(to)
				if err == nil {
					e.To = append(e.To, tt)
				} else {
					e.To = append(e.To, to)
				}
			}
			delete(hdrs, h)
		case h == HdrCC:
			for _, cc := range v {
				tcc, err := (&mime.WordDecoder{}).DecodeHeader(cc)
				if err == nil {
					e.Cc = append(e.Cc, tcc)
				} else {
					e.Cc = append(e.Cc, cc)
				}
			}
			delete(hdrs, h)
		case h == HdrBCC:
			for _, bcc := range v {
				tbcc, err := (&mime.WordDecoder{}).DecodeHeader(bcc)
				if err == nil {
					e.Bcc = append(e.Bcc, tbcc)
				} else {
					e.Bcc = append(e.Bcc, bcc)
				}
			}
			delete(hdrs, h)
		case h == HdrFrom:
			e.From = v[0]
			fr, err := (&mime.WordDecoder{}).DecodeHeader(e.From)
			if err == nil && len(fr) > 0 {
				e.From = fr
			}
			delete(hdrs, h)
		}
	}
	e.Headers = hdrs
	body := tp.R

	// Recursively parse the MIME parts
	ps, err := parseMIMEParts(e.Headers, body)
	if err != nil {
		return e, err
	}
	for _, p := range ps {
		if ct := p.header.Get(HdrContentType); ct == "" {
			return e, ErrMissingContentType
		}
		ct, _, err := mime.ParseMediaType(p.header.Get(HdrContentType))
		if err != nil {
			return e, err
		}
		switch {
		case ct == ContentTypePlain:
			e.Text = p.body
		case ct == ContentTypeHTML:
			e.HTML = p.body
		default:
			// Get filename for the attachment from the content disposition header.
			_, params, err := mime.ParseMediaType(p.header.Get(HdrContentDisposition))
			if err != nil {
				continue
			}
			attachment := Attachment{
				Filename: params["filename"],
				Content:  p.body,
				Header:   p.header,
			}
			e.Attachments = append(e.Attachments, attachment)
		}
	}
	return e, nil
}

func (e *Email) categorizeAttachments() (htmlRelated, others []Attachment) {
	for _, a := range e.Attachments {
		if a.HTMLRelated {
			htmlRelated = append(htmlRelated, a)
		} else {
			others = append(others, a)
		}
	}
	return
}

// Bytes converts the Email object to a []byte representation, including all
// needed MIMEHeaders, boundaries, etc.
func (e *Email) Bytes() ([]byte, error) {
	// TODO: better guess buffer size
	buff := bytes.NewBuffer(make([]byte, 0, 4096))

	headers, err := e.msgHeaders()
	if err != nil {
		return nil, err
	}

	htmlAttachments, otherAttachments := e.categorizeAttachments()
	if len(e.HTML) == 0 && len(htmlAttachments) > 0 {
		return nil, errors.New("there are HTML attachments, but no HTML body")
	}

	var (
		isMixed       = len(otherAttachments) > 0
		isAlternative = len(e.Text) > 0 && len(e.HTML) > 0
	)

	var w *multipart.Writer
	if isMixed || isAlternative {
		w = multipart.NewWriter(buff)
	}
	switch {
	case isMixed:
		headers.Set(HdrContentType, ContentTypeMultipartMixed+";\r\n boundary="+w.Boundary())
	case isAlternative:
		headers.Set(HdrContentType, ContentTypeMultipartAlt+";\r\n boundary="+w.Boundary())
	case len(e.HTML) > 0:
		headers.Set(HdrContentType, ContentTypeHTML+"; charset="+defaultCharEncoding)
		headers.Set(HdrContentTransferEncoding, contentEncQuotedPrintable)
	default:
		headers.Set(HdrContentType, ContentTypePlain+"; charset="+defaultCharEncoding)
		headers.Set(HdrContentTransferEncoding, contentEncQuotedPrintable)
	}
	headerToBytes(buff, headers)
	_, err = io.WriteString(buff, "\r\n")
	if err != nil {
		return nil, err
	}

	// Check to see if there is a Text or HTML field.
	if len(e.Text) > 0 || len(e.HTML) > 0 {
		var subWriter *multipart.Writer

		if isMixed && isAlternative {
			// Create the multipart alternative part.
			subWriter = multipart.NewWriter(buff)
			header := textproto.MIMEHeader{
				HdrContentType: {ContentTypeMultipartAlt + ";\r\n boundary=" + subWriter.Boundary()},
			}
			if _, err := w.CreatePart(header); err != nil {
				return nil, err
			}
		} else {
			subWriter = w
		}
		// Create the body sections.
		if len(e.Text) > 0 {
			// Write the text.
			if err := writeMessage(buff, e.Text, isMixed || isAlternative, ContentTypePlain, subWriter); err != nil {
				return nil, err
			}
		}
		if len(e.HTML) > 0 {
			messageWriter := subWriter
			var relatedWriter *multipart.Writer
			if len(htmlAttachments) > 0 {
				relatedWriter = multipart.NewWriter(buff)
				header := textproto.MIMEHeader{
					HdrContentType: {ContentTypeMultipartRelated + ";\r\n boundary=" + relatedWriter.Boundary()},
				}
				if _, err := subWriter.CreatePart(header); err != nil {
					return nil, err
				}

				messageWriter = relatedWriter
			}
			// Write the HTML.
			if err := writeMessage(buff, e.HTML, isMixed || isAlternative, ContentTypeHTML, messageWriter); err != nil {
				return nil, err
			}
			if len(htmlAttachments) > 0 {
				for _, a := range htmlAttachments {
					ap, err := relatedWriter.CreatePart(a.Header)
					if err != nil {
						return nil, err
					}
					// Write the base64Wrapped content to the part
					base64Wrap(ap, a.Content)
				}

				relatedWriter.Close()
			}
		}
		if isMixed && isAlternative {
			if err := subWriter.Close(); err != nil {
				return nil, err
			}
		}
	}

	// Create attachment part, if necessary.
	for _, a := range otherAttachments {
		ap, err := w.CreatePart(a.Header)
		if err != nil {
			return nil, err
		}
		// Write the base64Wrapped content to the part.
		base64Wrap(ap, a.Content)
	}

	if isMixed || isAlternative {
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	return buff.Bytes(), nil
}

// parseMIMEParts will recursively walk a MIME entity and return a []mime.Part containing
// each (flattened) mime.Part found.
// It is important to note that there are no limits to the number of recursions, so be
// careful when parsing unknown MIME structures!
func parseMIMEParts(hs textproto.MIMEHeader, b io.Reader) ([]*part, error) {
	var ps []*part
	// If no content type is given, set it to the default
	if _, ok := hs[HdrContentType]; !ok {
		hs.Set(HdrContentType, defaultContentType)
	}

	ct, params, err := mime.ParseMediaType(hs.Get(HdrContentType))
	if err != nil {
		return ps, err
	}

	// If it's a multipart email, recursively parse the parts
	if strings.HasPrefix(ct, "multipart/") {
		if _, ok := params[paramBoundary]; !ok {
			return ps, ErrMissingBoundary
		}
		mr := multipart.NewReader(b, params[paramBoundary])
		for {
			var buf bytes.Buffer
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return ps, err
			}
			if _, ok := p.Header[HdrContentType]; !ok {
				p.Header.Set(HdrContentType, defaultContentType)
			}
			subct, _, err := mime.ParseMediaType(p.Header.Get(HdrContentType))
			if err != nil {
				return ps, err
			}
			if strings.HasPrefix(subct, "multipart/") {
				sps, err := parseMIMEParts(p.Header, p)
				if err != nil {
					return ps, err
				}
				ps = append(ps, sps...)
			} else {
				var reader io.Reader
				reader = p

				const cte = HdrContentTransferEncoding
				if p.Header.Get(cte) == contentEncBase64 {
					reader = base64.NewDecoder(base64.StdEncoding, reader)
				}

				// Otherwise, just append the part to the list
				// Copy the part data into the buffer
				if _, err := io.Copy(&buf, reader); err != nil {
					return ps, err
				}
				ps = append(ps, &part{body: buf.Bytes(), header: p.Header})
			}
		}
	} else {
		// If it is not a multipart email, parse the body content as a single "part"
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, b); err != nil {
			return ps, err
		}
		ps = append(ps, &part{body: buf.Bytes(), header: hs})
	}
	return ps, nil
}

// Attach is used to attach content from an io.Reader to the email.
// Required parameters include an io.Reader, the desired filename for the attachment, and the Content-Type
// The function will return the created Attachment for reference, as well as nil for the error, if successful.
func (e *Email) Attach(r io.Reader, filename string, c string) (a Attachment, err error) {
	var buffer bytes.Buffer
	if _, err = io.Copy(&buffer, r); err != nil {
		return
	}

	at := Attachment{
		Filename: filename,
		Header:   textproto.MIMEHeader{},
		Content:  buffer.Bytes(),
	}

	if c != "" {
		at.Header.Set(HdrContentType, c)
	} else {
		at.Header.Set(HdrContentType, ContentTypeOctetStream)
	}

	at.Header.Set(HdrContentDisposition, fmt.Sprintf("attachment;\r\n filename=\"%s\"", filename))
	at.Header.Set(HdrContentID, fmt.Sprintf("<%s>", filename))
	at.Header.Set(HdrContentTransferEncoding, contentEncBase64)
	e.Attachments = append(e.Attachments, at)
	return at, nil
}

// AttachFile is used to attach content to the email.
// It attempts to open the file referenced by filename and, if successful, creates an Attachment.
// This Attachment is then appended to the slice of Email.Attachments.
// The function will then return the Attachment for reference, as well as nil for the error, if successful.
func (e *Email) AttachFile(filename string) (a Attachment, err error) {
	f, err := os.Open(filename)
	if err != nil {
		return
	}
	defer f.Close()

	ct := mime.TypeByExtension(filepath.Ext(filename))
	basename := filepath.Base(filename)
	return e.Attach(f, basename, ct)
}

// formatAddress formats the address as a valid RFC 5322 address.
// If the address's name contains non-ASCII characters
// the name will be rendered according to RFC 2047.
func (e *Email) formatAddress(addr string) (string, error) {
	em, err := mail.ParseAddress(addr)
	if err != nil {
		return addr, err
	}

	return em.String(), nil
}

// formatAddresses formats the list of address as a valid RFC 5322 addresses.
// If the address's name contains non-ASCII characters
// the name will be rendered according to RFC 2047.
func (e *Email) formatAddresses(addrs []string) ([]string, error) {
	fAddrs := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		em, err := e.formatAddress(addr)
		if err != nil {
			return nil, err
		}
		fAddrs = append(fAddrs, em)
	}

	return fAddrs, nil
}

// msgHeaders merges the Email's various fields and custom headers together in a
// standards compliant way to create a MIMEHeader to be used in the resulting
// message. It does not alter e.Headers.
//
// "e"'s fields To, Cc, From, Subject will be used unless they are present in
// e.Headers. Unless set in e.Headers, "Date" will filled with the current time.
func (e *Email) msgHeaders() (textproto.MIMEHeader, error) {
	res := make(textproto.MIMEHeader, len(e.Headers)+4)
	if e.Headers != nil {
		for _, h := range msgHeaders {
			if v, ok := e.Headers[h]; ok {
				res[h] = v
			}
		}
	}

	// Set headers if there are values.
	if _, ok := res[HdrReplyTo]; !ok && len(e.ReplyTo) > 0 {
		replyTo, err := e.formatAddresses(e.ReplyTo)
		if err != nil {
			return nil, err
		}
		res.Set(HdrReplyTo, strings.Join(replyTo, ", "))
	}
	if _, ok := res[HdrTo]; !ok && len(e.To) > 0 {
		to, err := e.formatAddresses(e.To)
		if err != nil {
			return nil, err
		}
		res.Set(HdrTo, strings.Join(to, ", "))
	}
	if _, ok := res[HdrCC]; !ok && len(e.Cc) > 0 {
		cc, err := e.formatAddresses(e.Cc)
		if err != nil {
			return nil, err
		}
		res.Set(HdrCC, strings.Join(cc, ", "))
	}
	if _, ok := res[HdrSubject]; !ok && e.Subject != "" {
		res.Set(HdrSubject, e.Subject)
	}
	if _, ok := res[HdrMessageID]; !ok {
		id, err := generateMessageID()
		if err != nil {
			return nil, err
		}
		res.Set(HdrMessageID, id)
	}
	// Date and From are required headers.
	if _, ok := res[HdrFrom]; !ok {
		from, err := e.formatAddress(e.From)
		if err != nil {
			return nil, err
		}
		res.Set(HdrFrom, from)
	}
	if _, ok := res[HdrDate]; !ok {
		res.Set(HdrDate, time.Now().Format(time.RFC1123Z))
	}
	if _, ok := res[HdrMimeVersion]; !ok {
		res.Set(HdrMimeVersion, defaultMimeVersion)
	}
	for field, vals := range e.Headers {
		if _, ok := res[field]; !ok {
			res[field] = vals
		}
	}
	return res, nil
}

func writeMessage(buff io.Writer, msg []byte, multipart bool, mediaType string, w *multipart.Writer) error {
	if multipart {
		header := textproto.MIMEHeader{
			HdrContentType:             {mediaType + "; charset=" + defaultCharEncoding},
			HdrContentTransferEncoding: {contentEncQuotedPrintable},
		}

		if _, err := w.CreatePart(header); err != nil {
			return err
		}
	}

	qp := quotedprintable.NewWriter(buff)

	// Write the text.
	if _, err := qp.Write(msg); err != nil {
		return err
	}
	return qp.Close()
}

// Select and parse an SMTP envelope sender address.  Choose Email.Sender if set,
// or fallback to Email.From.
func (e *Email) parseSender() (string, error) {
	if e.Sender != "" {
		sender, err := mail.ParseAddress(e.Sender)
		if err != nil {
			return "", err
		}
		return sender.Address, nil
	}

	from, err := mail.ParseAddress(e.From)
	if err != nil {
		return "", err
	}
	return from.Address, nil
}

// base64Wrap encodes the attachment content, and wraps it according to
// RFC 2045 standards (every 76 chars).
// The output is then written to the specified io.Writer.
func base64Wrap(w io.Writer, b []byte) {
	// 57 raw bytes per 76-byte base64 line.
	const maxRaw = 57

	// Buffer for each line, including trailing CRLF.
	buffer := make([]byte, MaxLineLength+len("\r\n"))
	copy(buffer[MaxLineLength:], "\r\n")

	// Process raw chunks until there's no longer enough to fill a line.
	for len(b) >= maxRaw {
		base64.StdEncoding.Encode(buffer, b[:maxRaw])
		w.Write(buffer)
		b = b[maxRaw:]
	}

	// Handle the last chunk of bytes.
	if len(b) > 0 {
		out := buffer[:base64.StdEncoding.EncodedLen(len(b))]
		base64.StdEncoding.Encode(out, b)
		out = append(out, "\r\n"...)
		w.Write(out)
	}
}

// headerToBytes renders "header" to "buff". If there are multiple values for a
// field, multiple "Field: value\r\n" lines will be emitted.
func headerToBytes(buff io.Writer, header textproto.MIMEHeader) {
	for field, vals := range header {
		for _, subval := range vals {
			// bytes.Buffer.Write() never returns an error.
			io.WriteString(buff, field)
			io.WriteString(buff, ": ")
			// Write the encoded header if needed
			switch {
			case field == HdrContentType || field == HdrContentDisposition:
				buff.Write([]byte(subval))
			default:
				buff.Write([]byte(mime.QEncoding.Encode(defaultCharEncoding, subval)))
			}
			io.WriteString(buff, "\r\n")
		}
	}
}

// generateMessageID generates and returns a string suitable for an RFC 2822
// compliant Message-ID, e.g.:
// <1444789264909237300.3464.1819418242800517193@DESKTOP01>
//
// The following parameters are used to generate a Message-ID:
// - The nanoseconds since Epoch
// - The calling PID
// - A cryptographically random int64
// - The sending hostname
func generateMessageID() (string, error) {
	t := time.Now().UnixNano()
	pid := os.Getpid()
	rint, err := rand.Int(rand.Reader, maxBigInt)
	if err != nil {
		return "", err
	}
	h, err := os.Hostname()

	// If there is no hostname or hostname is not an fqdn, use the default hostname.
	if err != nil || !strings.Contains(h, ".") {
		h = defaultHostname
	}

	return fmt.Sprintf("<%d.%d.%d@%s>", t, pid, rint, h), nil
}
