// Copyright 2012 Apcera Inc. All rights reserved.

package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apcera/gnatsd/hashmap"
	"github.com/apcera/gnatsd/sublist"
)

// The size of the bufio reader/writer on top of the socket.
const defaultBufSize = 32768

type client struct {
	mu   sync.Mutex
	cid  uint64
	opts clientOpts
	conn net.Conn
	bw   *bufio.Writer
	srv  *Server
	subs *hashmap.HashMap
	pcd  map[*client]struct{}
	atmr *time.Timer
	ptmr *time.Timer
	pout int
	parseState
	stats
}

func (c *client) String() string {
	return fmt.Sprintf("cid:%d", c.cid)
}

type subscription struct {
	client  *client
	subject []byte
	queue   []byte
	sid     []byte
	nm      int64
	max     int64
}

type clientOpts struct {
	Verbose       bool   `json:"verbose"`
	Pedantic      bool   `json:"pedantic"`
	SslRequired   bool   `json:"ssl_required"`
	Authorization string `json:"auth_token"`
	Username      string `json:"user"`
	Password      string `json:"pass"`
	Name          string `json:"name"`
}

var defaultOpts = clientOpts{Verbose: true, Pedantic: true}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func clientConnStr(conn net.Conn) interface{} {
	if ip, ok := conn.(*net.TCPConn); ok {
		addr := ip.RemoteAddr().(*net.TCPAddr)
		return []string{fmt.Sprintf("%v, %d", addr.IP, addr.Port)}
	}
	return "N/A"
}

func (c *client) readLoop() {
	// Grab the connection off the client, it will be cleared on a close.
	// We check for that after the loop, but want to avoid a nil dereference
	conn := c.conn
	if conn == nil {
		return
	}
	b := make([]byte, defaultBufSize)

	for {
		n, err := conn.Read(b)
		if err != nil {
			c.closeConnection()
			return
		}
		if err := c.parse(b[:n]); err != nil {
			Log(err.Error(), clientConnStr(c.conn), c.cid)
			c.sendErr("Parser Error")
			c.closeConnection()
			return
		}
		// Check pending clients for flush.
		for cp, _ := range c.pcd {
			// Flush those in the set
			cp.mu.Lock()
			if cp.conn != nil {
				cp.conn.SetWriteDeadline(time.Now().Add(DEFAULT_FLUSH_DEADLINE))
				err := cp.bw.Flush()
				cp.conn.SetWriteDeadline(time.Time{})
				if err != nil {
					Debugf("Error flushing: %v", err)
					cp.closeConnection()
				}
			}
			cp.mu.Unlock()
			delete(c.pcd, cp)
		}
		// Check to see if we got closed, e.g. slow consumer
		if c.conn == nil {
			return
		}
	}
}

func (c *client) traceMsg(msg []byte) {
	pm := fmt.Sprintf("Processing msg: %d", c.inMsgs)
	opa := []interface{}{pm, string(c.pa.subject), string(c.pa.reply), string(msg)}
	Trace(logStr(opa), fmt.Sprintf("c: %d", c.cid))
}

func (c *client) traceOp(op string, arg []byte) {
	if !trace {
		return
	}
	opa := []interface{}{fmt.Sprintf("%s OP", op)}
	if arg != nil {
		opa = append(opa, fmt.Sprintf("%s %s", op, string(arg)))
	}
	Trace(logStr(opa), fmt.Sprintf("c: %d", c.cid))
}

func (c *client) processConnect(arg []byte) error {
	c.traceOp("CONNECT", arg)

	// This will be resolved regardless before we exit this func,
	// so we can just clear it here.
	c.clearAuthTimer()

	// FIXME, check err
	if err := json.Unmarshal(arg, &c.opts); err != nil {
		return err
	}
	// Check for Auth
	if c.srv != nil {
		if ok := c.srv.checkAuth(c); !ok {
			c.sendErr("Authorization is Required")
			return fmt.Errorf("Authorization Error")
		}
	}
	if c.opts.Verbose {
		c.sendOK()
	}
	return nil
}

func (c *client) authViolation() {
	c.sendErr("Authorization is Required")
	c.closeConnection()
}

func (c *client) sendErr(err string) {
	c.mu.Lock()
	if c.bw != nil {
		c.bw.WriteString(fmt.Sprintf("-ERR '%s'\r\n", err))
		c.pcd[c] = needFlush
	}
	c.mu.Unlock()
}

func (c *client) sendOK() {
	c.mu.Lock()
	c.bw.WriteString("+OK\r\n")
	c.pcd[c] = needFlush
	c.mu.Unlock()
}

func (c *client) processPing() {
	c.traceOp("PING", nil)
	if c.conn == nil {
		return
	}
	c.mu.Lock()
	c.bw.WriteString("PONG\r\n")
	err := c.bw.Flush()
	if err != nil {
		c.clearConnection()
		Debug("Error on Flush", err, clientConnStr(c.conn), c.cid)
	}
	c.mu.Unlock()
}

func (c *client) processPong() {
	c.traceOp("PONG", nil)
	c.mu.Lock()
	c.pout -= 1
	c.mu.Unlock()
}

const argsLenMax = 3

func (c *client) processPub(arg []byte) error {
	if trace {
		c.traceOp("PUB", arg)
	}

	// Unroll splitArgs to avoid runtime/heap issues
	a := [argsLenMax][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t', '\r', '\n':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}

	switch len(args) {
	case 2:
		c.pa.subject = args[0]
		c.pa.reply = nil
		c.pa.size = parseSize(args[1])
		c.pa.szb = args[1]
	case 3:
		c.pa.subject = args[0]
		c.pa.reply = args[1]
		c.pa.size = parseSize(args[2])
		c.pa.szb = args[2]
	default:
		return fmt.Errorf("processPub Parse Error: '%s'", arg)
	}
	if c.pa.size < 0 {
		return fmt.Errorf("processPub Bad or Missing Size: '%s'", arg)
	}
	if c.opts.Pedantic && !sublist.IsValidLiteralSubject(c.pa.subject) {
		c.sendErr("Invalid Subject")
	}
	return nil
}

func splitArg(arg []byte) [][]byte {
	a := [argsLenMax][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t', '\r', '\n':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}
	return args
}

func (c *client) processSub(argo []byte) (err error) {
	c.traceOp("SUB", argo)
	// Copy so we do not reference a potentially large buffer
	arg := make([]byte, len(argo))
	copy(arg, argo)
	args := splitArg(arg)
	sub := &subscription{client: c}
	switch len(args) {
	case 2:
		sub.subject = args[0]
		sub.queue = nil
		sub.sid = args[1]
	case 3:
		sub.subject = args[0]
		sub.queue = args[1]
		sub.sid = args[2]
	default:
		return fmt.Errorf("processSub Parse Error: '%s'", arg)
	}

	c.mu.Lock()
	c.subs.Set(sub.sid, sub)
	if c.srv != nil {
		err = c.srv.sl.Insert(sub.subject, sub)
	}
	c.mu.Unlock()
	if err != nil {
		c.sendErr("Invalid Subject")
	} else if c.opts.Verbose {
		c.sendOK()
	}
	return nil
}

func (c *client) unsubscribe(sub *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub.max > 0 && sub.nm <= sub.max {
		return
	}
	c.traceOp("DELSUB", sub.sid)
	c.subs.Remove(sub.sid)
	if c.srv != nil {
		c.srv.sl.Remove(sub.subject, sub)
	}
}

func (c *client) processUnsub(arg []byte) error {
	c.traceOp("UNSUB", arg)
	args := splitArg(arg)
	var sid []byte
	max := -1

	switch len(args) {
	case 1:
		sid = args[0]
	case 2:
		sid = args[0]
		max = parseSize(args[1])
	default:
		return fmt.Errorf("processUnsub Parse Error: '%s'", arg)
	}
	if sub, ok := (c.subs.Get(sid)).(*subscription); ok {
		if max > 0 {
			sub.max = int64(max)
		}
		c.unsubscribe(sub)
	}
	if c.opts.Verbose {
		c.sendOK()
	}
	return nil
}

func (c *client) msgHeader(mh []byte, sub *subscription) []byte {
	mh = append(mh, sub.sid...)
	mh = append(mh, ' ')
	if c.pa.reply != nil {
		mh = append(mh, c.pa.reply...)
		mh = append(mh, ' ')
	}
	mh = append(mh, c.pa.szb...)
	mh = append(mh, "\r\n"...)
	return mh
}

// Used to treat map as efficient set
type empty struct{}

var needFlush = empty{}

func (c *client) deliverMsg(sub *subscription, mh, msg []byte) {
	if sub.client == nil || sub.client.conn == nil {
		return
	}
	client := sub.client
	client.mu.Lock()
	sub.nm++
	if sub.max > 0 && sub.nm > sub.max {
		client.mu.Unlock()
		client.unsubscribe(sub)
		return
	}

	// Update statistics
	client.outMsgs++
	client.outBytes += int64(len(msg))

	atomic.AddInt64(&c.srv.outMsgs, 1)
	atomic.AddInt64(&c.srv.outBytes, int64(len(msg)))

	// Check to see if our writes will cause a flush
	// in the underlying bufio. If so limit time we
	// will wait for flush to complete.

	deadlineSet := false
	if client.bw.Available() < (len(mh) + len(msg) + len(CR_LF)) {
		client.conn.SetWriteDeadline(time.Now().Add(DEFAULT_FLUSH_DEADLINE))
		deadlineSet = true
	}

	// Deliver to the client.
	_, err := client.bw.Write(mh)
	if err != nil {
		goto writeErr
	}

	_, err = client.bw.Write(msg)
	if err != nil {
		goto writeErr
	}

	// FIXME, this is already attached to original message
	_, err = client.bw.WriteString(CR_LF)
	if err != nil {
		goto writeErr
	}

	if deadlineSet {
		client.conn.SetWriteDeadline(time.Time{})
	}

	client.mu.Unlock()
	c.pcd[client] = needFlush
	return

writeErr:
	if deadlineSet {
		client.conn.SetWriteDeadline(time.Time{})
	}
	client.mu.Unlock()

	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		// FIXME: SlowConsumer logic
		Log("Slow Consumer Detected", clientConnStr(client.conn), client.cid)
		client.closeConnection()
	} else {
		Debugf("Error writing msg: %v", err)
	}
}

func (c *client) processMsg(msg []byte) {
	c.inMsgs++
	c.inBytes += int64(len(msg))

	if c.srv != nil {
		atomic.AddInt64(&c.srv.inMsgs, 1)
		atomic.AddInt64(&c.srv.inBytes, int64(len(msg)))
	}

	if trace {
		c.traceMsg(msg)
	}
	if c.srv == nil {
		return
	}
	if c.opts.Verbose {
		c.sendOK()
	}

	scratch := [512]byte{}
	msgh := scratch[:0]

	r := c.srv.sl.Match(c.pa.subject)
	if len(r) <= 0 {
		return
	}

	// msg header
	// FIXME, put MSG into initializer
	msgh = append(msgh, "MSG "...)
	msgh = append(msgh, c.pa.subject...)
	msgh = append(msgh, ' ')
	si := len(msgh)

	var qmap map[string][]*subscription
	var qsubs []*subscription

	for _, v := range r {
		sub := v.(*subscription)
		if sub.queue != nil {
			// FIXME, this can be more efficient
			if qmap == nil {
				qmap = make(map[string][]*subscription)
			}
			//qname := *(*string)(unsafe.Pointer(&sub.queue))
			qname := string(sub.queue)
			qsubs = qmap[qname]
			if qsubs == nil {
				qsubs = make([]*subscription, 0, 4)
			}
			qsubs = append(qsubs, sub)
			qmap[qname] = qsubs
			continue
		}
		mh := c.msgHeader(msgh[:si], sub)
		c.deliverMsg(sub, mh, msg)
	}
	if qmap != nil {
		for _, qsubs := range qmap {
			index := rand.Int() % len(qsubs)
			sub := qsubs[index]
			mh := c.msgHeader(msgh[:si], sub)
			c.deliverMsg(sub, mh, msg)
		}
	}
}

func (c *client) processPingTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ptmr = nil
	// Check if we are ready yet..
	if _, ok := c.conn.(*net.TCPConn); !ok {
		return
	}

	Debug("Ping Timer", clientConnStr(c.conn), c.cid)

	// Check for violation
	c.pout += 1
	if c.pout > c.srv.opts.MaxPingsOut {
		Debug("Stale Connection - Closing", clientConnStr(c.conn), c.cid)
		if c.bw != nil {
			c.bw.WriteString(fmt.Sprintf("-ERR '%s'\r\n", "Stale Connection"))
			c.bw.Flush()
		}
		c.clearConnection()
		return
	}

	// Send PING
	c.bw.WriteString("PING\r\n")
	err := c.bw.Flush()
	if err != nil {
		Debug("Error on Flush", err, clientConnStr(c.conn), c.cid)
		c.clearConnection()
	} else {
		// Reset to fire again if all OK.
		c.setPingTimer()
	}
}

func (c *client) setPingTimer() {
	if c.srv == nil {
		return
	}
	d := c.srv.opts.PingInterval
	c.ptmr = time.AfterFunc(d, func() { c.processPingTimer() })
}

// Lock should be held
func (c *client) clearPingTimer() {
	if c.ptmr == nil {
		return
	}
	c.ptmr.Stop()
	c.ptmr = nil
}

func (c *client) setAuthTimer(d time.Duration) {
	c.atmr = time.AfterFunc(d, func() { c.authViolation() })
}

// Lock should be held
func (c *client) clearAuthTimer() {
	if c.atmr == nil {
		return
	}
	c.atmr.Stop()
	c.atmr = nil
}

// Lock should be held
func (c *client) clearConnection() {
	if c.conn == nil {
		return
	}
	c.bw.Flush()
	c.conn.Close()
	c.conn = nil
}

func (c *client) closeConnection() {
	if c.conn == nil {
		return
	}
	Debug("Client connection closed", clientConnStr(c.conn), c.cid)

	c.mu.Lock()
	c.clearAuthTimer()
	c.clearPingTimer()
	c.clearConnection()
	subs := c.subs.All()
	c.mu.Unlock()

	if c.srv != nil {
		// Unregister
		c.srv.removeClient(c)

		// Remove subscriptions.
		for _, s := range subs {
			if sub, ok := s.(*subscription); ok {
				c.srv.sl.Remove(sub.subject, sub)
			}
		}
	}
}