package main

import (
	"errors"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
)

func main() {
	serverConnection, clientConnection := net.Pipe()
	serverResult := make(chan error, 1)
	go serveSMTP(serverConnection, serverResult)

	client, err := smtp.NewClient(clientConnection, "mail.example")
	if err != nil {
		panic(err)
	}
	if err := client.Hello("cg12.local"); err != nil {
		panic(err)
	}
	if err := client.Mail("compiler@cg12.local"); err != nil {
		panic(err)
	}
	if err := client.Rcpt("runtime@example.com"); err != nil {
		panic(err)
	}
	message, err := client.Data()
	if err != nil {
		panic(err)
	}
	if _, err := io.WriteString(message, "Subject: status\r\n\r\ncompiled\r\n.leading dot\r\n"); err != nil {
		panic(err)
	}
	if err := message.Close(); err != nil {
		panic(err)
	}
	if err := client.Quit(); err != nil {
		panic(err)
	}
	if err := <-serverResult; err != nil {
		panic(err)
	}
}

func serveSMTP(connection net.Conn, result chan<- error) {
	protocol := textproto.NewConn(connection)
	defer protocol.Close()

	if err := protocol.PrintfLine("220 mail.example ESMTP cg12"); err != nil {
		result <- err
		return
	}
	if err := expectSMTPLine(protocol, "EHLO cg12.local"); err != nil {
		result <- err
		return
	}
	if err := protocol.PrintfLine("250-mail.example"); err != nil {
		result <- err
		return
	}
	if err := protocol.PrintfLine("250 8BITMIME"); err != nil {
		result <- err
		return
	}

	line, err := protocol.ReadLine()
	if err != nil {
		result <- err
		return
	}
	if !strings.HasPrefix(line, "MAIL FROM:<compiler@cg12.local>") {
		result <- errors.New("SMTP MAIL command mismatch")
		return
	}
	if err := protocol.PrintfLine("250 sender accepted"); err != nil {
		result <- err
		return
	}
	if err := expectSMTPLine(protocol, "RCPT TO:<runtime@example.com>"); err != nil {
		result <- err
		return
	}
	if err := protocol.PrintfLine("250 recipient accepted"); err != nil {
		result <- err
		return
	}
	if err := expectSMTPLine(protocol, "DATA"); err != nil {
		result <- err
		return
	}
	if err := protocol.PrintfLine("354 send message"); err != nil {
		result <- err
		return
	}

	message, err := protocol.ReadDotBytes()
	if err != nil {
		result <- err
		return
	}
	text := string(message)
	if !strings.Contains(text, "Subject: status\n\ncompiled\n.leading dot\n") {
		result <- errors.New("SMTP message body mismatch")
		return
	}
	if err := protocol.PrintfLine("250 queued"); err != nil {
		result <- err
		return
	}
	if err := expectSMTPLine(protocol, "QUIT"); err != nil {
		result <- err
		return
	}
	if err := protocol.PrintfLine("221 closing"); err != nil {
		result <- err
		return
	}
	result <- nil
}

func expectSMTPLine(protocol *textproto.Conn, expected string) error {
	line, err := protocol.ReadLine()
	if err != nil {
		return err
	}
	if line != expected {
		return errors.New("SMTP command mismatch: got " + line + ", want " + expected)
	}
	return nil
}
