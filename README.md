<a href="https://zerodha.tech"><img src="https://zerodha.tech/static/images/github-badge.svg" align="right" /></a>

smtppool
========

smtppool is a Go library that creates a pool of reusable SMTP connections for high throughput e-mailing. It gracefully handles idle connections, timeouts, and retries. The e-mail formatting, parsing, and preparation code is forked from [jordan-wright/email](https://github.com/jordan-wright/email).


### Install
```go get github.com/knadh/smtppool/v2```


#### Usage
```go
package main

import (
	"log"
	"time"

	"github.com/knadh/smtppool/v2"
)

func main() {
	// Try https://github.com/mailhog/MailHog for running a local dummy SMTP server.
	// Create a new pool.
	pool, err := smtppool.New(smtppool.Opt{
		Host:            "localhost",
		Port:            1025,
		MaxConns:        10,
		IdleTimeout:     time.Second * 10,
		PoolWaitTimeout: time.Second * 3,
		SSL:             smtppool.SSLNone
	})
	if err != nil {
		log.Fatalf("error creating pool: %v", err)
	}

	e:= smtppool.Email{
		From:    "John Doe <john@example.com>",
		To:      []string{"doe@example.com"},

		// Optional.
		Bcc:     []string{"doebcc@example.com"},
		Cc:      []string{"doecc@example.com"},

		Subject: "Hello, World",
		Text:    []byte("This is a test e-mail"),
		HTML:    []byte("<strong>This is a test e-mail</strong>"),
	}

	// Add attachments.
	if _, err := e.AttachFile("test.txt"); err != nil {
		log.Fatalf("error attaching file: %v", err)
	}

	if err := pool.Send(e); err != nil {
		log.Fatalf("error sending e-mail: %v", err)
	}
}
```

Licensed under the MIT license.
