package zlib

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"regexp"
	"time"

	"ztools/ztls"
)

var smtpEndRegex = regexp.MustCompile(`(?:\r\n)|^[0-9]{3} .+\r\n$`)
var pop3EndRegex = regexp.MustCompile(`(?:\r\n\.\r\n$)|(?:\r\n$)`)
var imapStatusEndRegex = regexp.MustCompile(`\r\n$`)

const (
	SMTP_COMMAND = "STARTTLS\r\n"
	POP3_COMMAND = "STLS\r\n"
	IMAP_COMMAND = "a001 STARTTLS\r\n"
)

// Implements the net.Conn interface
type Conn struct {
	// Underlying network connection
	conn    net.Conn
	tlsConn *ztls.Conn
	isTls   bool

	// Max TLS version
	maxTlsVersion uint16

	// Keep track of state / network operations
	operations []ConnectionEvent

	// Cache the deadlines so we can reapply after TLS handshake
	readDeadline  time.Time
	writeDeadline time.Time

	caPool  *x509.CertPool
	onlyCBC bool

	domain string
}

func (c *Conn) getUnderlyingConn() net.Conn {
	if c.isTls {
		return c.tlsConn
	}
	return c.conn
}

func (c *Conn) SetCBCOnly() {
	c.onlyCBC = true
}

func (c *Conn) SetCAPool(pool *x509.CertPool) {
	c.caPool = pool
}

func (c *Conn) SetDomain(domain string) {
	c.domain = domain
}

// Layer in the regular conn methods
func (c *Conn) LocalAddr() net.Addr {
	return c.getUnderlyingConn().LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.getUnderlyingConn().RemoteAddr()
}

func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline = t
	c.writeDeadline = t
	return c.getUnderlyingConn().SetDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return c.getUnderlyingConn().SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.getUnderlyingConn().SetWriteDeadline(t)
}

// Delegate here, but record all the things
func (c *Conn) Write(b []byte) (int, error) {
	n, err := c.getUnderlyingConn().Write(b)
	w := WriteEvent{Sent: b}
	event := ConnectionEvent{
		Data:  &w,
		Error: err,
	}
	c.operations = append(c.operations, event)
	return n, err
}

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.getUnderlyingConn().Read(b)
	r := ReadEvent{
		Response: b[0:n],
	}
	c.appendEvent(&r, err)
	return n, err
}

func (c *Conn) Close() error {
	return c.getUnderlyingConn().Close()
}

// Extra method - Do a TLS Handshake and record progress
func (c *Conn) TLSHandshake() error {
	if c.isTls {
		return fmt.Errorf(
			"Attempted repeat handshake with remote host %s",
			c.RemoteAddr().String())
	}
	tlsConfig := new(ztls.Config)
	tlsConfig.InsecureSkipVerify = true
	tlsConfig.MinVersion = ztls.VersionSSL30
	tlsConfig.MaxVersion = c.maxTlsVersion
	tlsConfig.RootCAs = c.caPool
	if c.domain != "" {
		tlsConfig.ServerName = c.domain
	}
	if c.onlyCBC {
		tlsConfig.CipherSuites = ztls.CBCSuiteIDList
	}
	c.tlsConn = ztls.Client(c.conn, tlsConfig)
	c.tlsConn.SetReadDeadline(c.readDeadline)
	c.tlsConn.SetWriteDeadline(c.writeDeadline)
	c.isTls = true
	err := c.tlsConn.Handshake()
	hl := c.tlsConn.GetHandshakeLog()
	ts := TLSHandshakeEvent{handshakeLog: hl}
	event := ConnectionEvent{
		Data:  &ts,
		Error: err,
	}
	c.operations = append(c.operations, event)
	return err
}

func (c *Conn) sendStartTLSCommand(command string) error {
	// Don't doublehandshake
	if c.isTls {
		return fmt.Errorf(
			"Attempt STARTTLS after TLS handshake with remote host %s",
			c.RemoteAddr().String())
	}
	// Send the STARTTLS message
	starttls := []byte(command)
	_, err := c.conn.Write(starttls)
	return err
}

// Do a STARTTLS handshake
func (c *Conn) SMTPStartTLSHandshake() error {
	// Make the state
	ss := StartTLSEvent{Command: SMTP_COMMAND}

	// Send the command
	if err := c.sendStartTLSCommand(SMTP_COMMAND); err != nil {
		c.appendEvent(&ss, err)
		return err
	}
	// Read the response on a successful send
	buf := make([]byte, 256)
	n, err := c.readSmtpResponse(buf)
	ss.Response = buf[0:n]

	// Record everything no matter the result
	c.appendEvent(&ss, err)

	// Stop if we failed already
	if err != nil {
		return err
	}
	// Successful so far, attempt to do the actual handshake
	return c.TLSHandshake()
}

func (c *Conn) POP3StartTLSHandshake() error {
	ss := StartTLSEvent{Command: POP3_COMMAND}
	if err := c.sendStartTLSCommand(POP3_COMMAND); err != nil {
		c.appendEvent(&ss, err)
		return err
	}

	buf := make([]byte, 512)
	n, err := c.readPop3Response(buf)
	ss.Response = buf[0:n]
	c.appendEvent(&ss, err)

	if err != nil {
		return err
	}
	return c.TLSHandshake()
}

func (c *Conn) IMAPStartTLSHandshake() error {
	ss := StartTLSEvent{Command: IMAP_COMMAND}
	if err := c.sendStartTLSCommand(IMAP_COMMAND); err != nil {
		c.appendEvent(&ss, err)
		return err
	}

	buf := make([]byte, 512)
	n, err := c.readImapStatusResponse(buf)
	ss.Response = buf[0:n]
	c.appendEvent(&ss, err)

	if err != nil {
		return err
	}
	return c.TLSHandshake()
}

func (c *Conn) readUntilRegex(res []byte, expr *regexp.Regexp) (int, error) {
	buf := res[0:]
	length := 0
	for finished := false; !finished; {
		n, err := c.getUnderlyingConn().Read(buf)
		length += n
		if err != nil {
			return length, err
		}
		if expr.Match(res[0:length]) {
			finished = true
		}
		if length == len(res) {
			return length, errors.New("Not enough buffer space")
		}
		buf = res[length:]
	}
	return length, nil
}

func (c *Conn) readSmtpResponse(res []byte) (int, error) {
	return c.readUntilRegex(res, smtpEndRegex)
}

func (c *Conn) SMTPBanner(b []byte) (int, error) {
	n, err := c.readSmtpResponse(b)
	mb := MailBannerEvent{}
	mb.Banner = string(b[0:n])
	c.appendEvent(&mb, err)
	return n, err
}

func (c *Conn) EHLO(domain string) error {
	cmd := []byte("EHLO " + domain + "\r\n")
	ee := EHLOEvent{}
	if _, err := c.getUnderlyingConn().Write(cmd); err != nil {
		c.appendEvent(&ee, err)
		return err
	}

	buf := make([]byte, 512)
	n, err := c.readSmtpResponse(buf)
	ee.Response = buf[0:n]
	c.appendEvent(&ee, err)
	return err
}

func (c *Conn) SMTPHelp() error {
	cmd := []byte("HELP\r\n")
	h := new(SMTPHelpEvent)
	if _, err := c.getUnderlyingConn().Write(cmd); err != nil {
		c.appendEvent(h, err)
		return err
	}
	buf := make([]byte, 512)
	n, err := c.readSmtpResponse(buf)
	h.Response = buf[0:n]
	c.appendEvent(h, err)
	return err
}

func (c *Conn) readPop3Response(res []byte) (int, error) {
	return c.readUntilRegex(res, pop3EndRegex)
}

func (c *Conn) POP3Banner(b []byte) (int, error) {
	n, err := c.readPop3Response(b)
	mb := MailBannerEvent{
		Banner: string(b[0:n]),
	}
	c.appendEvent(&mb, err)
	return n, err
}

func (c *Conn) readImapStatusResponse(res []byte) (int, error) {
	return c.readUntilRegex(res, imapStatusEndRegex)
}

func (c *Conn) IMAPBanner(b []byte) (int, error) {
	n, err := c.readImapStatusResponse(b)
	mb := MailBannerEvent{
		Banner: string(b[0:n]),
	}
	c.appendEvent(&mb, err)
	return n, err
}

func (c *Conn) CheckHeartbleed(b []byte) (int, error) {
	if !c.isTls {
		return 0, fmt.Errorf(
			"Must perform TLS handshake before sending Heartbleed probe to %s",
			c.RemoteAddr().String())
	}
	n, err := c.tlsConn.CheckHeartbleed(b)
	hb := c.tlsConn.GetHeartbleedLog()
	if err == ztls.HeartbleedError {
		err = nil
	}
	event := ConnectionEvent{
		Data:  &HeartbleedEvent{heartbleedLog: hb},
		Error: err,
	}
	c.operations = append(c.operations, event)
	return n, err
}

func (c *Conn) SendModbusEcho(b []byte) (huh int, err error) {
	req := ModbusRequest {
		Function: ModbusFunctionEncapsulatedInterface,
		Data: []byte {
			0x0E, // read device info
			0x01, // product code
			0x00, // object id, should always be 0 in initial request
		},
	}

	data, err := req.MarshalBinary()
	written, err := c.Write(data) // TODO verify write
	if err != nil || written != len(data) {
		return
	}

	res, err := c.GetModbusResponse()

	if res.Function.IsException() {
		//TODO should convert to ModbusException
	} else {
		//TODO log this
	}

	// make sure the whole thing gets appended to the operation log
	// e.g. c.appendEvent(modbusEvent, modbusError)
	return
}

func (c *Conn) States() []ConnectionEvent {
	return c.operations
}

func (c *Conn) appendEvent(data EventData, err error) {
	event := ConnectionEvent{
		Data:  data,
		Error: err,
	}
	c.operations = append(c.operations, event)
}
