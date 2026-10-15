// This file was forked from:
// https://github.com/jordan-wright/email (MIT License, Copyright (c) 2013 Jordan Wright).
package smtppool

import (
	"bufio"
	"strings"
	"testing"

	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"unicode/utf8"
)

func prepareEmail() Email {
	e := Email{}
	e.From = "Jordan Wright <test@example.com>"
	e.To = []string{"Bécassine <test@example.com>"}
	e.Bcc = []string{"test_bcc@example.com"}
	e.Cc = []string{"test_cc@example.com"}
	e.Subject = "Awesome Subject"
	return e
}

func basicTests(t *testing.T, e Email) *mail.Message {
	raw, err := e.Bytes()
	if err != nil {
		t.Fatal("Failed to render message: ", e)
	}

	msg, err := mail.ReadMessage(bytes.NewBuffer(raw))
	if err != nil {
		t.Fatal("Could not parse rendered message: ", err)
	}

	toAddr := mail.Address{
		Name:    "Bécassine",
		Address: "test@example.com",
	}

	fromAddr := mail.Address{
		Name:    "Jordan Wright",
		Address: "test@example.com",
	}

	ccAddr := mail.Address{
		Name:    "",
		Address: "test_cc@example.com",
	}

	expectedHeaders := map[string]string{
		"To":      toAddr.String(),
		"From":    fromAddr.String(),
		"Cc":      ccAddr.String(),
		"Subject": "Awesome Subject",
	}

	for header, expected := range expectedHeaders {
		if val := msg.Header.Get(header); val != expected {
			t.Errorf("Wrong value for message header %s: %v != %v", header, val, expected)
		}
	}
	return msg
}

func TestEmailText(t *testing.T) {
	e := prepareEmail()
	e.Text = []byte("Text Body is, of course, supported!\n")

	msg := basicTests(t, e)

	// Were the right headers set?
	ct := msg.Header.Get("Content-type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal("Content-type header is invalid: ", ct)
	} else if mt != "text/plain" {
		t.Fatalf("Content-type expected \"text/plain\", not %v", mt)
	}
}

func TestEmailLongReferences(t *testing.T) {
	for _, count := range []int{0, 1, 8, 15, 20, 200} {
		for _, idBytes := range []int{30, 66, 74, 75, 76, 77, 78, 90, 130, 990} {
			for _, trailing := range []string{"", " ", "  ", "\t", " \t "} {
				for _, repeated := range []bool{false, true} {
					name := fmt.Sprintf("count=%d/bytes=%d/trailing=%q/repeated=%t", count, idBytes, trailing, repeated)
					t.Run(name, func(t *testing.T) {
						ids := make([]string, count)
						for i := range count {
							index := i
							if repeated {
								index = 0
							}
							ids[i] = "<" + fmt.Sprintf("%03d", index) + strings.Repeat("a", idBytes-15) + "@example.com>"
						}
						references := strings.Join(ids, " ") + trailing
						e := prepareEmail()
						e.Text = []byte("Reply.")
						e.Headers = textproto.MIMEHeader{"References": {references}}
						if len(ids) > 0 {
							e.Headers.Set("In-Reply-To", ids[len(ids)-1])
						}
						raw, err := e.Bytes()
						if err != nil {
							t.Fatal(err)
						}
						checkHeaderLines(t, raw)
						msg, err := mail.ReadMessage(bytes.NewReader(raw))
						if err != nil {
							t.Fatal(err)
						}
						if got := msg.Header.Get("References"); got != strings.Join(ids, " ") {
							t.Error("References changed after parsing")
						}
						if len(ids) > 0 && msg.Header.Get("In-Reply-To") != ids[len(ids)-1] {
							t.Error("In-Reply-To changed after parsing")
						}
						if e.Headers.Get("References") != references {
							t.Error("input References changed")
						}
						body, err := io.ReadAll(msg.Body)
						if err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(body, e.Text) {
							t.Errorf("body = %q, want %q", body, e.Text)
						}
					})
				}
			}
		}
	}
}

func TestHeaderToBytesFolding(t *testing.T) {
	cases := []struct {
		name         string
		field        string
		value        string
		maxLineBytes int
	}{
		{name: "short", field: HdrSubject, value: "A short subject"},
		{name: "empty", field: "References"},
		{name: "76 bytes", field: HdrSubject, value: strings.Repeat("a", 67)},
		{name: "77 bytes", field: HdrSubject, value: strings.Repeat("a", 68)},
		{name: "78 bytes", field: HdrSubject, value: strings.Repeat("a", 69)},
		{name: "79 bytes", field: HdrSubject, value: strings.Repeat("a", 70)},
		{name: "long subject", field: HdrSubject, value: strings.Repeat("word ", 300) + "end"},
		{name: "spaces and tabs", field: HdrSubject, value: strings.Repeat("word \t  ", 30) + "end"},
		{name: "unicode", field: HdrSubject, value: strings.Repeat("Résumé ", 100) + "end"},
		{name: "long message ID", field: "References", value: "<" + strings.Repeat("a", 120) + "@example.com>", maxLineBytes: 998},
		{name: "repeated IDs", field: "References", value: strings.Repeat("<message@example.com> ", 60) + "<message@example.com>"},
		{name: "folded content type", field: HdrContentType, value: "multipart/mixed;\r\n boundary=example"},
		{name: "unicode 77 byte regression", field: HdrSubject, value: "é" + strings.Repeat("a", 50)},
		{name: "unicode 78 byte regression", field: HdrSubject, value: "é" + strings.Repeat("a", 51)},
		{name: "emoji", field: HdrSubject, value: strings.Repeat("🙂 report ", 30)},
		{name: "non Latin", field: HdrSubject, value: strings.Repeat("报告 ", 40)},
		{name: "trailing space after long ID", field: "References", value: "<" + strings.Repeat("a", 75) + "@example.com> ", maxLineBytes: 998},
		{name: "trailing tabs after long ID", field: "References", value: "<" + strings.Repeat("a", 75) + "@example.com>\t\t", maxLineBytes: 998},
		{name: "trailing mixed whitespace", field: "References", value: "<" + strings.Repeat("a", 75) + "@example.com> \t  ", maxLineBytes: 998},
		{name: "long trailing whitespace", field: HdrSubject, value: "word" + strings.Repeat(" ", 200), maxLineBytes: 998},
		{name: "long leading whitespace", field: HdrSubject, value: strings.Repeat(" ", 200) + "word", maxLineBytes: 998},
		{name: "long internal whitespace", field: HdrSubject, value: "first" + strings.Repeat(" \t", 60) + "last", maxLineBytes: 998},
		{name: "whitespace only value", field: "X-Example", value: strings.Repeat(" ", 200), maxLineBytes: 998},
		{name: "tabs only value", field: "X-Example", value: strings.Repeat("\t", 200), maxLineBytes: 998},
		{name: "folded long MIME value", field: HdrContentDisposition, value: "attachment;\r\n filename=\"" + strings.Repeat("monthly report ", 20) + "end.txt\""},
		{name: "MIME quotes and escapes", field: HdrContentDisposition, value: "attachment; filename=\"" + strings.Repeat("annual report ", 20) + "quoted\\\"name.txt\""},
		{name: "folded disposition", field: HdrContentDisposition, value: "attachment;\r\n filename=\"monthly report.txt\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			headerToBytes(&raw, textproto.MIMEHeader{tc.field: {tc.value}})
			checkHeaderLines(t, append(bytes.Clone(raw.Bytes()), []byte("\r\n")...))
			limit := tc.maxLineBytes
			if limit == 0 {
				limit = 76
			}
			for i, line := range strings.Split(strings.TrimSuffix(raw.String(), "\r\n"), "\r\n") {
				if len(line) > limit {
					t.Errorf("header line has %d bytes, limit is %d", len(line), limit)
				}
				if i > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
					t.Errorf("continuation line has no leading whitespace: %q", line)
				}
			}
			unfolded := strings.ReplaceAll(strings.TrimSuffix(raw.String(), "\r\n"), "\r\n", "")
			encoded, ok := strings.CutPrefix(unfolded, tc.field+": ")
			if !ok {
				t.Fatalf("unexpected header: %q", raw.String())
			}
			decoded, err := (&mime.WordDecoder{}).DecodeHeader(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.ReplaceAll(tc.value, "\r\n", ""); decoded != want {
				t.Errorf("unfolded value = %q, want %q", decoded, want)
			}
			expectedValue := tc.value
			if tc.field != HdrContentType && tc.field != HdrContentDisposition {
				expectedValue = mime.QEncoding.Encode(defaultCharEncoding, expectedValue)
			}
			if len(tc.field)+2+len(expectedValue) <= 76 && !strings.Contains(expectedValue, "\r\n") {
				if want := tc.field + ": " + expectedValue + "\r\n"; raw.String() != want {
					t.Errorf("short header = %q, want %q", raw.String(), want)
				}
			} else if tc.maxLineBytes == 0 && !strings.Contains(strings.TrimSuffix(raw.String(), "\r\n"), "\r\n") {
				t.Error("long header was not folded")
			}
		})
	}
}

func TestHeaderToBytesMultipleValues(t *testing.T) {
	values := []string{strings.Repeat("first ", 30) + "end", strings.Repeat("second ", 30) + "end"}
	var raw bytes.Buffer
	headerToBytes(&raw, textproto.MIMEHeader{"X-Example": values})
	raw.WriteString("\r\n")
	headers, err := textproto.NewReader(bufio.NewReader(&raw)).ReadMIMEHeader()
	if err != nil {
		t.Fatal(err)
	}
	got := headers.Values("X-Example")
	if len(got) != len(values) {
		t.Fatalf("got %d header values, want %d", len(got), len(values))
	}
	for i := range values {
		if got[i] != values[i] {
			t.Errorf("header value %d = %q, want %q", i, got[i], values[i])
		}
	}
}

func TestEmailEncodedSubjectBoundaries(t *testing.T) {
	for _, text := range []string{"é", "报告", "🙂", "Résumé "} {
		for length := range 161 {
			for _, subject := range []string{text + strings.Repeat("a", length), strings.Repeat("a", length) + text} {
				name := fmt.Sprintf("text=%q/length=%d/subject=%q", text, length, subject)
				t.Run(name, func(t *testing.T) {
					e := prepareEmail()
					e.Subject = subject
					raw, err := e.Bytes()
					if err != nil {
						t.Fatal(err)
					}
					checkHeaderLines(t, raw)
					msg, err := mail.ReadMessage(bytes.NewReader(raw))
					if err != nil {
						t.Fatal(err)
					}
					got, err := (&mime.WordDecoder{}).DecodeHeader(msg.Header.Get(HdrSubject))
					if err != nil {
						t.Fatal(err)
					}
					if got != subject {
						t.Errorf("subject = %q, want %q", got, subject)
					}
				})
			}
		}
	}
}

func TestHeaderToBytesEmptyHeaders(t *testing.T) {
	cases := []textproto.MIMEHeader{nil, {}, {"X-Example": nil}, {"X-Example": {}}}
	for i, header := range cases {
		t.Run(fmt.Sprintf("case=%d", i), func(t *testing.T) {
			var raw bytes.Buffer
			headerToBytes(&raw, header)
			if raw.Len() != 0 {
				t.Errorf("empty headers produced %q", raw.String())
			}
		})
	}
}

func FuzzWriteFoldedHeader(f *testing.F) {
	for _, value := range []string{
		"", "Subject: short", "Subject: " + strings.Repeat("a", 68),
		"References: " + strings.Repeat("<id@example.com>\t", 100),
		"X: " + strings.Repeat("word ", 100) + strings.Repeat(" \t", 1000),
		strings.Repeat(" \t", 1000), strings.Repeat(" ", 200) + "word",
		"X: " + strings.Repeat("a", 1200) + " next",
		"Content-Type: text/plain;\r\n name=\"" + strings.Repeat("word ", 100) + "end\"",
		"X: first\r\n\r\n\tsecond\r\n",
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 8192 {
			t.Skip()
		}

		var folded, again bytes.Buffer
		writeFoldedHeader(&folded, value)
		if strings.ReplaceAll(folded.String(), "\r\n", "") != strings.ReplaceAll(value, "\r\n", "") {
			t.Fatal("folding changed the unfolded value")
		}

		writeFoldedHeader(&again, folded.String())
		if !bytes.Equal(folded.Bytes(), again.Bytes()) {
			t.Fatal("folding is not idempotent")
		}

		// Check each original line separately so existing breaks (including
		// invalid ones) are distinguished from newly inserted folds.
		var joined bytes.Buffer
		for i, line := range strings.Split(value, "\r\n") {
			if i > 0 {
				joined.WriteString("\r\n")
			}

			var part bytes.Buffer
			writeFoldedHeader(&part, line)
			joined.Write(part.Bytes())
			for j, continuation := range strings.Split(part.String(), "\r\n") {
				text := strings.Trim(continuation, " \t")
				if j > 0 && (text == "" || (continuation[0] != ' ' && continuation[0] != '\t')) {
					t.Fatalf("invalid continuation: %q", continuation)
				}

				// Very long lines are only necessary for indivisible words or
				// whitespaces. Subsequent word must break to a new line.
				if len(strings.TrimRight(continuation, " \t")) > headerLineLength && strings.ContainsAny(text, " \t") {
					t.Fatalf("missed fold in %q", continuation)
				}
			}
		}

		if !bytes.Equal(folded.Bytes(), joined.Bytes()) {
			t.Fatal("folding changed existing line breaks")
		}
	})
}

func FuzzHeaderToBytesFolding(f *testing.F) {
	for _, value := range []string{
		"", "short", "é" + strings.Repeat("a", 50), "é" + strings.Repeat("a", 51),
		strings.Repeat("word ", 300), strings.Repeat(" \t", 60),
		"first" + strings.Repeat(" \t", 60) + "last",
		strings.Repeat("a", 75) + " ", strings.Repeat("a", 75) + "\t\t",
		strings.Repeat("Résumé 🙂 报告 ", 40),
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 4096 || !utf8.ValidString(value) || strings.Contains(value, "=?") {
			t.Skip()
		}
		whitespaceRun, wordRun := 0, 0
		for _, r := range value {
			if r != '\t' && (r < 32 || r == 127) {
				t.Skip()
			}
			if r == ' ' || r == '\t' {
				whitespaceRun++
				wordRun = 0
			} else {
				wordRun += utf8.RuneLen(r)
				whitespaceRun = 0
			}
			if whitespaceRun > 200 || wordRun > 200 {
				t.Skip()
			}
		}
		var raw bytes.Buffer
		headerToBytes(&raw, textproto.MIMEHeader{HdrSubject: {value}})
		checkHeaderLines(t, append(bytes.Clone(raw.Bytes()), []byte("\r\n")...))
		unfolded := strings.ReplaceAll(strings.TrimSuffix(raw.String(), "\r\n"), "\r\n", "")
		encoded, ok := strings.CutPrefix(unfolded, HdrSubject+": ")
		if !ok {
			t.Fatal("Subject header changed")
		}
		decoded, err := (&mime.WordDecoder{}).DecodeHeader(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if decoded != value {
			t.Errorf("decoded value = %q, want %q", decoded, value)
		}
	})
}

func TestEmailWithHTMLAttachments(t *testing.T) {
	e := prepareEmail()

	// Set plain text to exercise "mime/alternative"
	e.Text = []byte("Text Body is, of course, supported!\n")

	e.HTML = []byte("<html><body>This is a text.</body></html>")

	// Set HTML attachment to exercise "mime/related".
	_, err := e.Attach(bytes.NewBufferString("Rad attachment"), "rad.txt", "image/png; charset=utf-8")
	if err != nil {
		t.Fatal("Could not add an attachment to the message: ", err)
	}
	// Attach returns a copy, so mark the attachment stored on the email.
	e.Attachments[len(e.Attachments)-1].HTMLRelated = true

	b, err := e.Bytes()
	if err != nil {
		t.Fatal("Could not serialize e-mail:", err)
	}

	// Print the bytes for ocular validation and make sure no errors.
	//fmt.Println(string(b))

	// TODO: Verify the attachments.
	s := trimReader{rd: bytes.NewBuffer(b)}
	tp := textproto.NewReader(bufio.NewReader(s))
	// Parse the main headers
	hdrs, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatal("Could not parse the headers:", err)
	}
	// Recursively parse the MIME parts
	ps, err := parseMIMEParts(hdrs, tp.R)
	if err != nil {
		t.Fatal("Could not parse the MIME parts recursively:", err)
	}

	plainTextFound := false
	htmlFound := false
	imageFound := false
	if expected, actual := 3, len(ps); actual != expected {
		t.Error("Unexpected number of parts. Expected:", expected, "Was:", actual)
	}
	for _, part := range ps {
		// part has "header" and "body []byte"
		ct := part.header.Get("Content-Type")
		if strings.Contains(ct, "image/png") {
			imageFound = true
		}
		if strings.Contains(ct, "text/html") {
			htmlFound = true
		}
		if strings.Contains(ct, "text/plain") {
			plainTextFound = true
		}
	}

	if !plainTextFound {
		t.Error("Did not find plain text part.")
	}
	if !htmlFound {
		t.Error("Did not find HTML part.")
	}
	if !imageFound {
		t.Error("Did not find image part.")
	}
}

func TestEmailHTMLOnlyWithRelatedAttachment(t *testing.T) {
	e := prepareEmail()
	e.HTML = []byte("<html><body><img src=\"cid:image.png\"></body></html>")

	_, err := e.Attach(bytes.NewBufferString("image content"), "image.png", "image/png")
	if err != nil {
		t.Fatal("Could not add an attachment to the message: ", err)
	}
	e.Attachments[len(e.Attachments)-1].HTMLRelated = true

	raw, err := e.Bytes()
	if err != nil {
		t.Fatal("Could not serialize e-mail: ", err)
	}

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal("Could not parse rendered message: ", err)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get(HdrContentType))
	if err != nil {
		t.Fatal("Could not parse message Content-Type: ", err)
	}
	if mediaType != ContentTypeMultipartRelated {
		t.Fatalf("Content-Type expected %q, not %q", ContentTypeMultipartRelated, mediaType)
	}

	related := multipart.NewReader(msg.Body, params[paramBoundary])
	htmlPart, err := related.NextPart()
	if err != nil {
		t.Fatal("Could not read HTML part: ", err)
	}
	if contentType, _, err := mime.ParseMediaType(htmlPart.Header.Get(HdrContentType)); err != nil {
		t.Fatal("Could not parse HTML part Content-Type: ", err)
	} else if contentType != ContentTypeHTML {
		t.Fatalf("HTML part Content-Type expected %q, not %q", ContentTypeHTML, contentType)
	}

	attachmentPart, err := related.NextPart()
	if err != nil {
		t.Fatal("Could not read related attachment: ", err)
	}
	if contentType, _, err := mime.ParseMediaType(attachmentPart.Header.Get(HdrContentType)); err != nil {
		t.Fatal("Could not parse attachment Content-Type: ", err)
	} else if contentType != "image/png" {
		t.Fatalf("Attachment Content-Type expected %q, not %q", "image/png", contentType)
	}

	if _, err := related.NextPart(); err != io.EOF {
		t.Fatalf("Expected exactly two related parts, got %v", err)
	}
}

func TestEmailHTML(t *testing.T) {
	e := prepareEmail()
	e.HTML = []byte("<h1>Fancy Html is supported, too!</h1>\n")

	msg := basicTests(t, e)

	// Were the right headers set?
	ct := msg.Header.Get("Content-type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("Content-type header is invalid: %#v", ct)
	} else if mt != "text/html" {
		t.Fatalf("Content-type expected \"text/html\", not %v", mt)
	}
}

func TestEmailTextAttachment(t *testing.T) {
	e := prepareEmail()
	e.Text = []byte("Text Body is, of course, supported!\n")
	_, err := e.Attach(bytes.NewBufferString("Rad attachment"), "rad.txt", "text/plain; charset=utf-8")
	if err != nil {
		t.Fatal("Could not add an attachment to the message: ", err)
	}

	msg := basicTests(t, e)

	// Were the right headers set?
	ct := msg.Header.Get("Content-type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal("Content-type header is invalid: ", ct)
	} else if mt != "multipart/mixed" {
		t.Fatalf("Content-type expected \"multipart/mixed\", not %v", mt)
	}
	b := params["boundary"]
	if b == "" {
		t.Fatalf("Invalid or missing boundary parameter: %#v", b)
	}
	if len(params) != 1 {
		t.Fatal("Unexpected content-type parameters")
	}

	// Is the generated message parsable?
	mixed := multipart.NewReader(msg.Body, params["boundary"])

	text, err := mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find text component of email: %s", err)
	}

	// Does the text portion match what we expect?
	mt, _, err = mime.ParseMediaType(text.Header.Get("Content-type"))
	if err != nil {
		t.Fatal("Could not parse message's Content-Type")
	} else if mt != "text/plain" {
		t.Fatal("Message missing text/plain")
	}
	plainText, err := ioutil.ReadAll(text)
	if err != nil {
		t.Fatal("Could not read plain text component of message: ", err)
	}
	if !bytes.Equal(plainText, []byte("Text Body is, of course, supported!\r\n")) {
		t.Fatalf("Plain text is broken: %#q", plainText)
	}

	// Check attachments.
	_, err = mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find attachment component of email: %s", err)
	}

	if _, err = mixed.NextPart(); err != io.EOF {
		t.Error("Expected only text and one attachment!")
	}
}

func TestEmailTextHtmlAttachment(t *testing.T) {
	e := prepareEmail()
	e.Text = []byte("Text Body is, of course, supported!\n")
	e.HTML = []byte("<h1>Fancy Html is supported, too!</h1>\n")
	_, err := e.Attach(bytes.NewBufferString("Rad attachment"), "rad.txt", "text/plain; charset=utf-8")
	if err != nil {
		t.Fatal("Could not add an attachment to the message: ", err)
	}

	msg := basicTests(t, e)

	// Were the right headers set?
	ct := msg.Header.Get("Content-type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal("Content-type header is invalid: ", ct)
	} else if mt != "multipart/mixed" {
		t.Fatalf("Content-type expected \"multipart/mixed\", not %v", mt)
	}
	b := params["boundary"]
	if b == "" {
		t.Fatal("Unexpected empty boundary parameter")
	}
	if len(params) != 1 {
		t.Fatal("Unexpected content-type parameters")
	}

	// Is the generated message parsable?
	mixed := multipart.NewReader(msg.Body, params["boundary"])

	text, err := mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find text component of email: %s", err)
	}

	// Does the text portion match what we expect?
	mt, params, err = mime.ParseMediaType(text.Header.Get("Content-type"))
	if err != nil {
		t.Fatal("Could not parse message's Content-Type")
	} else if mt != "multipart/alternative" {
		t.Fatal("Message missing multipart/alternative")
	}
	mpReader := multipart.NewReader(text, params["boundary"])
	part, err := mpReader.NextPart()
	if err != nil {
		t.Fatal("Could not read plain text component of message: ", err)
	}
	plainText, err := ioutil.ReadAll(part)
	if err != nil {
		t.Fatal("Could not read plain text component of message: ", err)
	}
	if !bytes.Equal(plainText, []byte("Text Body is, of course, supported!\r\n")) {
		t.Fatalf("Plain text is broken: %#q", plainText)
	}

	// Check attachments.
	_, err = mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find attachment component of email: %s", err)
	}

	if _, err = mixed.NextPart(); err != io.EOF {
		t.Error("Expected only text and one attachment!")
	}
}

func TestEmailAttachment(t *testing.T) {
	e := prepareEmail()
	_, err := e.Attach(bytes.NewBufferString("Rad attachment"), "rad.txt", "text/plain; charset=utf-8")
	if err != nil {
		t.Fatal("Could not add an attachment to the message: ", err)
	}
	msg := basicTests(t, e)

	// Were the right headers set?
	ct := msg.Header.Get("Content-type")
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatal("Content-type header is invalid: ", ct)
	} else if mt != "multipart/mixed" {
		t.Fatalf("Content-type expected \"multipart/mixed\", not %v", mt)
	}
	b := params["boundary"]
	if b == "" {
		t.Fatal("Unexpected empty boundary parameter")
	}
	if len(params) != 1 {
		t.Fatal("Unexpected content-type parameters")
	}

	// Is the generated message parsable?
	mixed := multipart.NewReader(msg.Body, params["boundary"])

	// Check attachments.
	_, err = mixed.NextPart()
	if err != nil {
		t.Fatalf("Could not find attachment component of email: %s", err)
	}

	if _, err = mixed.NextPart(); err != io.EOF {
		t.Error("Expected only one attachment!")
	}
}

func TestEmailFromReader(t *testing.T) {
	ex := Email{
		Subject: "Test Subject",
		To:      []string{"Jordan Wright <jmwright798@gmail.com>"},
		From:    "Jordan Wright <jmwright798@gmail.com>",
		Text:    []byte("This is a test email with HTML Formatting. It also has very long lines so\nthat the content must be wrapped if using quoted-printable decoding.\n"),
		HTML:    []byte("<div dir=\"ltr\">This is a test email with <b>HTML Formatting.</b>\u00a0It also has very long lines so that the content must be wrapped if using quoted-printable decoding.</div>\n"),
	}
	raw := []byte(`
	MIME-Version: 1.0
Subject: Test Subject
From: Jordan Wright <jmwright798@gmail.com>
To: Jordan Wright <jmwright798@gmail.com>
Content-Type: multipart/alternative; boundary=001a114fb3fc42fd6b051f834280

--001a114fb3fc42fd6b051f834280
Content-Type: text/plain; charset=UTF-8

This is a test email with HTML Formatting. It also has very long lines so
that the content must be wrapped if using quoted-printable decoding.

--001a114fb3fc42fd6b051f834280
Content-Type: text/html; charset=UTF-8
Content-Transfer-Encoding: quoted-printable

<div dir=3D"ltr">This is a test email with <b>HTML Formatting.</b>=C2=A0It =
also has very long lines so that the content must be wrapped if using quote=
d-printable decoding.</div>

--001a114fb3fc42fd6b051f834280--`)
	e, err := NewEmailFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error creating email %s", err.Error())
	}
	if e.Subject != ex.Subject {
		t.Fatalf("Incorrect subject. %#q != %#q", e.Subject, ex.Subject)
	}
	if !bytes.Equal(e.Text, ex.Text) {
		t.Fatalf("Incorrect text: %#q != %#q", e.Text, ex.Text)
	}
	if !bytes.Equal(e.HTML, ex.HTML) {
		t.Fatalf("Incorrect HTML: %#q != %#q", e.HTML, ex.HTML)
	}
	if e.From != ex.From {
		t.Fatalf("Incorrect \"From\": %#q != %#q", e.From, ex.From)
	}

}

func TestNonAsciiEmailFromReader(t *testing.T) {
	ex := Email{
		Subject: "Test Subject",
		To:      []string{"Anaïs <anais@example.org>"},
		Cc:      []string{"Patrik Fältström <paf@example.com>"},
		From:    "Mrs ValÃ©rie Dupont <valerie.dupont@example.com>",
		Text:    []byte("This is a test message!"),
	}
	raw := []byte(`
	MIME-Version: 1.0
Subject: =?UTF-8?Q?Test Subject?=
From: Mrs =?ISO-8859-1?Q?Val=C3=A9rie=20Dupont?= <valerie.dupont@example.com>
To: =?utf-8?q?Ana=C3=AFs?= <anais@example.org>
Cc: =?ISO-8859-1?Q?Patrik_F=E4ltstr=F6m?= <paf@example.com>
Content-type: text/plain; charset=ISO-8859-1

This is a test message!`)

	e, err := NewEmailFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error creating email %s", err.Error())
	}
	if e.Subject != ex.Subject {
		t.Fatalf("Incorrect subject. %#q != %#q", e.Subject, ex.Subject)
	}
	if e.From != ex.From {
		t.Fatalf("Incorrect \"From\": %#q != %#q", e.From, ex.From)
	}
	if e.To[0] != ex.To[0] {
		t.Fatalf("Incorrect \"To\": %#q != %#q", e.To, ex.To)
	}
	if e.Cc[0] != ex.Cc[0] {
		t.Fatalf("Incorrect \"Cc\": %#q != %#q", e.Cc, ex.Cc)
	}
}

func TestNonMultipartEmailFromReader(t *testing.T) {
	ex := Email{
		Text:    []byte("This is a test message!"),
		Subject: "Example Subject (no MIME Type)",
		Headers: textproto.MIMEHeader{},
	}
	ex.Headers.Add("Content-Type", "text/plain; charset=us-ascii")
	ex.Headers.Add("Message-ID", "<foobar@example.com>")
	raw := []byte(`From: "Foo Bar" <foobar@example.com>
Content-Type: text/plain
To: foobar@example.com
Subject: Example Subject (no MIME Type)
Message-ID: <foobar@example.com>

This is a test message!`)
	e, err := NewEmailFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error creating email %s", err.Error())
	}
	if ex.Subject != e.Subject {
		t.Errorf("Incorrect subject. %#q != %#q\n", ex.Subject, e.Subject)
	}
	if !bytes.Equal(ex.Text, e.Text) {
		t.Errorf("Incorrect body. %#q != %#q\n", ex.Text, e.Text)
	}
	if ex.Headers.Get("Message-ID") != e.Headers.Get("Message-ID") {
		t.Errorf("Incorrect message ID. %#q != %#q\n", ex.Headers.Get("Message-ID"), e.Headers.Get("Message-ID"))
	}
}

func TestBase64EmailFromReader(t *testing.T) {
	ex := Email{
		Subject: "Test Subject",
		To:      []string{"Jordan Wright <jmwright798@gmail.com>"},
		From:    "Jordan Wright <jmwright798@gmail.com>",
		Text:    []byte("This is a test email with HTML Formatting. It also has very long lines so that the content must be wrapped if using quoted-printable decoding."),
		HTML:    []byte("<div dir=\"ltr\">This is a test email with <b>HTML Formatting.</b>\u00a0It also has very long lines so that the content must be wrapped if using quoted-printable decoding.</div>\n"),
	}
	raw := []byte(`
		MIME-Version: 1.0
Subject: Test Subject
From: Jordan Wright <jmwright798@gmail.com>
To: Jordan Wright <jmwright798@gmail.com>
Content-Type: multipart/alternative; boundary=001a114fb3fc42fd6b051f834280

--001a114fb3fc42fd6b051f834280
Content-Type: text/plain; charset=UTF-8
Content-Transfer-Encoding: base64

VGhpcyBpcyBhIHRlc3QgZW1haWwgd2l0aCBIVE1MIEZvcm1hdHRpbmcuIEl0IGFsc28gaGFzIHZl
cnkgbG9uZyBsaW5lcyBzbyB0aGF0IHRoZSBjb250ZW50IG11c3QgYmUgd3JhcHBlZCBpZiB1c2lu
ZyBxdW90ZWQtcHJpbnRhYmxlIGRlY29kaW5nLg==

--001a114fb3fc42fd6b051f834280
Content-Type: text/html; charset=UTF-8
Content-Transfer-Encoding: quoted-printable

<div dir=3D"ltr">This is a test email with <b>HTML Formatting.</b>=C2=A0It =
also has very long lines so that the content must be wrapped if using quote=
d-printable decoding.</div>

--001a114fb3fc42fd6b051f834280--`)
	e, err := NewEmailFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error creating email %s", err.Error())
	}
	if e.Subject != ex.Subject {
		t.Fatalf("Incorrect subject. %#q != %#q", e.Subject, ex.Subject)
	}
	if !bytes.Equal(e.Text, ex.Text) {
		t.Fatalf("Incorrect text: %#q != %#q", e.Text, ex.Text)
	}
	if !bytes.Equal(e.HTML, ex.HTML) {
		t.Fatalf("Incorrect HTML: %#q != %#q", e.HTML, ex.HTML)
	}
	if e.From != ex.From {
		t.Fatalf("Incorrect \"From\": %#q != %#q", e.From, ex.From)
	}
}

func Test_base64Wrap(t *testing.T) {
	file := "I'm a file long enough to force the function to wrap a\n" +
		"couple of lines, but I stop short of the end of one line and\n" +
		"have some padding dangling at the end."
	encoded := "SSdtIGEgZmlsZSBsb25nIGVub3VnaCB0byBmb3JjZSB0aGUgZnVuY3Rpb24gdG8gd3JhcCBhCmNv\r\n" +
		"dXBsZSBvZiBsaW5lcywgYnV0IEkgc3RvcCBzaG9ydCBvZiB0aGUgZW5kIG9mIG9uZSBsaW5lIGFu\r\n" +
		"ZApoYXZlIHNvbWUgcGFkZGluZyBkYW5nbGluZyBhdCB0aGUgZW5kLg==\r\n"

	var buf bytes.Buffer
	base64Wrap(&buf, []byte(file))
	if !bytes.Equal(buf.Bytes(), []byte(encoded)) {
		t.Fatalf("Encoded file does not match expected: %#q != %#q", string(buf.Bytes()), encoded)
	}
}

// *Since the mime library in use by ```email``` is now in the stdlib, this test is deprecated
func Test_quotedPrintEncode(t *testing.T) {
	var buf bytes.Buffer
	text := []byte("Dear reader!\n\n" +
		"This is a test email to try and capture some of the corner cases that exist within\n" +
		"the quoted-printable encoding.\n" +
		"There are some wacky parts like =, and this input assumes UNIX line breaks so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's a fish: 🐟\n")
	expected := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist=\r\n" +
		" within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =3D, and this input assumes UNIX line break=\r\n" +
		"s so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's=\r\n" +
		" a fish: =F0=9F=90=9F\r\n")
	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write(text); err != nil {
		t.Fatal("quotePrintEncode: ", err)
	}
	if err := qp.Close(); err != nil {
		t.Fatal("Error closing writer", err)
	}
	if b := buf.Bytes(); !bytes.Equal(b, expected) {
		t.Errorf("quotedPrintEncode generated incorrect results: %#q != %#q", b, expected)
	}
}

func TestMultipartNoContentType(t *testing.T) {
	raw := []byte(`From: Mikhail Gusarov <dottedmag@dottedmag.net>
To: notmuch@notmuchmail.org
References: <20091117190054.GU3165@dottiness.seas.harvard.edu>
Date: Wed, 18 Nov 2009 01:02:38 +0600
Message-ID: <87iqd9rn3l.fsf@vertex.dottedmag>
MIME-Version: 1.0
Subject: Re: [notmuch] Working with Maildir storage?
Content-Type: multipart/mixed; boundary="===============1958295626=="

--===============1958295626==
Content-Type: multipart/signed; boundary="=-=-=";
    micalg=pgp-sha1; protocol="application/pgp-signature"

--=-=-=
Content-Transfer-Encoding: quoted-printable

Twas brillig at 14:00:54 17.11.2009 UTC-05 when lars@seas.harvard.edu did g=
yre and gimble:

--=-=-=
Content-Type: application/pgp-signature

-----BEGIN PGP SIGNATURE-----
Version: GnuPG v1.4.9 (GNU/Linux)

iQIcBAEBAgAGBQJLAvNOAAoJEJ0g9lA+M4iIjLYQAKp0PXEgl3JMOEBisH52AsIK
=/ksP
-----END PGP SIGNATURE-----
--=-=-=--

--===============1958295626==
Content-Type: text/plain; charset="us-ascii"
MIME-Version: 1.0
Content-Transfer-Encoding: 7bit
Content-Disposition: inline

Testing!
--===============1958295626==--
`)
	e, err := NewEmailFromReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Error when parsing email %s", err.Error())
	}
	if !bytes.Equal(e.Text, []byte("Testing!")) {
		t.Fatalf("Error incorrect text: %#q != %#q\n", e.Text, "Testing!")
	}
}

// *Since the mime library in use by ```email``` is now in the stdlib, this test is deprecated
func Test_quotedPrintDecode(t *testing.T) {
	text := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist=\r\n" +
		" within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =3D, and this input assumes UNIX line break=\r\n" +
		"s so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's=\r\n" +
		" a fish: =F0=9F=90=9F\r\n")
	expected := []byte("Dear reader!\r\n\r\n" +
		"This is a test email to try and capture some of the corner cases that exist within\r\n" +
		"the quoted-printable encoding.\r\n" +
		"There are some wacky parts like =, and this input assumes UNIX line breaks so\r\n" +
		"it can come out a little weird.  Also, we need to support unicode so here's a fish: 🐟\r\n")
	qp := quotedprintable.NewReader(bytes.NewReader(text))
	got, err := ioutil.ReadAll(qp)
	if err != nil {
		t.Fatal("quotePrintDecode: ", err)
	}

	if !bytes.Equal(got, expected) {
		t.Errorf("quotedPrintDecode generated incorrect results: %#q != %#q", got, expected)
	}
}

func Benchmark_base64Wrap(b *testing.B) {
	// Reasonable base case; 128K random bytes
	file := make([]byte, 128*1024)
	if _, err := rand.Read(file); err != nil {
		panic(err)
	}
	for i := 0; i <= b.N; i++ {
		base64Wrap(ioutil.Discard, file)
	}
}

func TestParseSender(t *testing.T) {
	var cases = []struct {
		e      Email
		want   string
		haserr bool
	}{
		{
			Email{From: "from@test.com"},
			"from@test.com",
			false,
		},
		{
			Email{Sender: "sender@test.com", From: "from@test.com"},
			"sender@test.com",
			false,
		},
		{
			Email{Sender: "bad_address_sender"},
			"",
			true,
		},
		{
			Email{Sender: "good@sender.com", From: "bad_address_from"},
			"good@sender.com",
			false,
		},
	}

	for i, testcase := range cases {
		got, err := testcase.e.parseSender()
		if got != testcase.want || (err != nil) != testcase.haserr {
			t.Errorf(`%d: got %s != want %s or error "%t" != "%t"`, i+1, got, testcase.want, err != nil, testcase.haserr)
		}
	}
}

func checkHeaderLines(t *testing.T, raw []byte) {
	t.Helper()
	header, _, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatal("missing header separator")
	}
	for _, line := range bytes.Split(header, []byte("\r\n")) {
		if len(line) > 998 {
			t.Errorf("header line has %d bytes, limit is 998", len(line))
		}
		if len(bytes.Trim(line, " \t")) == 0 {
			t.Error("header contains a whitespace only line")
		}
		if bytes.ContainsAny(line, "\r\n") {
			t.Error("header contains a bare line break")
		}
		if bytes.Contains(line, []byte("=?UTF-8?")) && len(line) > 76 {
			t.Errorf("encoded header line has %d bytes, limit is 76", len(line))
		}
	}
}
