// Copyright 2020 lesismal. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package arpc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// TimeZero definition
	TimeZero time.Duration = 0
	// TimeForever definition
	TimeForever time.Duration = 1<<63 - 1
)

type rpcSession struct {
	seq  uint64
	done chan Message
}

type asyncHandler struct {
	h func(*Context)
	t *time.Timer
}

// Client defines rpc client struct
type Client struct {
	mux sync.RWMutex

	running      bool
	reconnecting bool
	chSend       chan Message

	seq             uint64
	sessionMap      map[uint64]*rpcSession
	asyncHandlerMap map[uint64]*asyncHandler

	onStop         func() int64
	onConnected    func(*Client)
	onDisConnected func(*Client)

	Conn    net.Conn
	Reader  io.Reader
	head    [HeadLen]byte
	Head    Header
	Codec   Codec
	Handler Handler
	Dialer  func() (net.Conn, error)
}

// OnConnected registers callback on connected
func (c *Client) OnConnected(onConnected func(*Client)) {
	c.onConnected = onConnected
}

// OnDisconnected registers callback on disconnected
func (c *Client) OnDisconnected(onDisConnected func(*Client)) {
	c.onDisConnected = onDisConnected
}

// Run client
func (c *Client) Run() {
	c.mux.Lock()
	defer c.mux.Unlock()
	if !c.running {
		c.running = true
		c.chSend = make(chan Message, c.Handler.SendQueueSize())
		go c.sendLoop()
		go c.recvLoop()
	}
}

// Stop client
func (c *Client) Stop() {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.running {
		c.running = false
		c.Conn.Close()
		close(c.chSend)
		if c.onStop != nil {
			c.onStop()
		}
		if c.onDisConnected != nil {
			c.onDisConnected(c)
		}
	}
}

// Call make rpc call
func (c *Client) Call(method string, req interface{}, rsp interface{}, timeout time.Duration) error {
	if !c.running {
		return ErrClientStopped
	}
	if c.reconnecting {
		return ErrClientReconnecting
	}

	if timeout <= 0 {
		return fmt.Errorf("invalid timeout arg: %v", timeout)
	}

	timer := time.NewTimer(timeout)

	msg := c.newReqMessage(method, req, 0)

	seq := msg.Seq()
	sess := sessionGet(seq)
	c.addSession(seq, sess)
	defer func() {
		timer.Stop()
		c.mux.Lock()
		delete(c.sessionMap, seq)
		sessionPut(sess)
		c.mux.Unlock()
	}()

	select {
	case c.chSend <- msg:
	case <-timer.C:
		return ErrClientTimeout
	}

	select {
	// response msg
	case msg = <-sess.done:
		defer memPut(msg)
	case <-timer.C:
		return ErrClientTimeout
	}

	switch msg.Cmd() {
	case RPCCmdRsp:
		switch vt := rsp.(type) {
		case *string:
			*vt = string(msg[HeadLen:])
		case *[]byte:
			*vt = msg[HeadLen:]
		case *error:
			*vt = errors.New(bytesToStr(msg[HeadLen:]))
		default:
			return c.Codec.Unmarshal(msg[HeadLen:], rsp)
		}
	case RPCCmdErr:
		return errors.New(string(msg[HeadLen:]))
	default:
	}

	return nil
}

// CallAsync make async rpc call
func (c *Client) CallAsync(method string, req interface{}, h func(*Context), timeout time.Duration) error {
	var (
		msg     = c.newReqMessage(method, req, 1)
		seq     = msg.Seq()
		handler *asyncHandler
	)

	if !c.running {
		return ErrClientStopped
	}
	if c.reconnecting {
		return ErrClientReconnecting
	}

	if h != nil {
		handler = asyncHandlerGet(h)
		c.addAsyncHandler(seq, handler)
	}

	switch timeout {
	// should not block forever
	// case TimeForever:
	// 	c.chSend <- msg
	// 	msg.Retain()
	case TimeZero:
		select {
		case c.chSend <- msg:
			msg.Retain()
		default:
			msg.Release()
			c.deleteAsyncHandler(seq)
			return ErrClientQueueIsFull
		}
	default:
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case c.chSend <- msg:
			msg.Retain()
			if h != nil {
				// timeout * 2: [push to send queue] + [recv response]
				handler.t = time.AfterFunc(timeout, func() {
					c.deleteAsyncHandler(seq)
				})
			}
		case <-timer.C:
			msg.Release()
			c.deleteAsyncHandler(seq)
			return ErrClientTimeout
		}
	}

	return nil
}

// Notify make rpc notify
func (c *Client) Notify(method string, data interface{}, timeout time.Duration) error {
	return c.CallAsync(method, data, nil, timeout)
}

// PushMsg push msg to client's send queue
func (c *Client) PushMsg(msg Message, timeout time.Duration) error {
	if !c.running {
		return ErrClientStopped
	}
	if c.reconnecting {
		return ErrClientReconnecting
	}

	if timeout < 0 {
		timeout = TimeForever
	}

	switch timeout {
	case TimeForever:
		c.chSend <- msg
		msg.Retain()
	case TimeZero:
		select {
		case c.chSend <- msg:
			msg.Retain()
		default:
			return ErrClientQueueIsFull
		}
	default:
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case c.chSend <- msg:
			msg.Retain()
		case <-timer.C:
			return ErrClientTimeout
		}
	}

	return nil
}

func (c *Client) addSession(seq uint64, session *rpcSession) {
	c.mux.Lock()
	c.sessionMap[seq] = session
	c.mux.Unlock()
}

func (c *Client) getSession(seq uint64) (*rpcSession, bool) {
	c.mux.Lock()
	session, ok := c.sessionMap[seq]
	c.mux.Unlock()
	return session, ok
}

func (c *Client) deleteSession(seq uint64) {
	c.mux.Lock()
	delete(c.sessionMap, seq)
	c.mux.Unlock()
}

func (c *Client) addAsyncHandler(seq uint64, h *asyncHandler) {
	c.mux.Lock()
	c.asyncHandlerMap[seq] = h
	c.mux.Unlock()
}

func (c *Client) deleteAsyncHandler(seq uint64) {
	c.mux.Lock()
	handler, ok := c.asyncHandlerMap[seq]
	if ok {
		delete(c.asyncHandlerMap, seq)
		asyncHandlerPut(handler)
	}
	c.mux.Unlock()
}

func (c *Client) getAndDeleteAsyncHandler(seq uint64) (*asyncHandler, bool) {
	c.mux.Lock()
	handler, ok := c.asyncHandlerMap[seq]
	if ok {
		delete(c.asyncHandlerMap, seq)
		asyncHandlerPut(handler)
	}
	c.mux.Unlock()
	return handler, ok
}

func (c *Client) recvLoop() {
	var (
		err  error
		msg  Message
		addr = c.Conn.RemoteAddr()
	)

	if c.Dialer == nil {
		// DefaultLogger.Info("[ARPC SVR] Client\t%v\trecvLoop start", c.Conn.RemoteAddr())
		// defer DefaultLogger.Info("[ARPC SVR] Client\t%v\trecvLoop stop", c.Conn.RemoteAddr())
		for c.running {
			msg, err = c.Handler.Recv(c)
			if err != nil {
				DefaultLogger.Info("[ARPC SVR] Client\t%v\tDisconnected: %v", addr, err)
				c.Stop()
				return
			}
			c.Handler.OnMessage(c, msg)
		}
	} else {
		// DefaultLogger.Info("[ARPC CLI]\t%v\trecvLoop start", c.Conn.RemoteAddr())
		// defer DefaultLogger.Info("[ARPC CLI]\t%v\trecvLoop stop", c.Conn.RemoteAddr())
		for c.running {
			for {
				msg, err = c.Handler.Recv(c)
				if err != nil {
					DefaultLogger.Info("[ARPC CLI]\t%v\tDisconnected: %v", addr, err)
					break
				}
				c.Handler.OnMessage(c, msg)
			}

			c.reconnecting = true
			c.Conn.Close()
			c.Conn = nil

			for c.running {
				DefaultLogger.Info("[ARPC CLI]\t%v\tReconnecting ...", addr)
				c.Conn, err = c.Dialer()
				if err == nil {
					DefaultLogger.Info("[ARPC CLI]\t%v\tConnected", addr)
					c.Reader = c.Handler.WrapReader(c.Conn)

					c.reconnecting = false

					if c.onConnected != nil {
						go safe(func() {
							c.onConnected(c)
						})
					}

					break
				}

				time.Sleep(time.Second)
			}
		}
	}

}

func (c *Client) sendLoop() {
	// if c.Dialer == nil {
	// 	DefaultLogger.Info("[ARPC SVR] Client\t%v\tsendLoop start", c.Conn.RemoteAddr())
	// 	defer DefaultLogger.Info("[ARPC SVR] Client\t%v\tsendLoop stop", c.Conn.RemoteAddr())
	// } else {
	// 	DefaultLogger.Info("[ARPC CLI]\t%v\tsendLoop start", c.Conn.RemoteAddr())
	// 	defer DefaultLogger.Info("[ARPC CLI]\t%v\tsendLoop stop", c.Conn.RemoteAddr())
	// }
	var conn net.Conn
	for msg := range c.chSend {
		conn = c.Conn
		if !c.reconnecting {
			c.Handler.Send(conn, msg.Payload())
		}
		msg.Release()
	}
}

func (c *Client) newReqMessage(method string, req interface{}, async byte) Message {
	var (
		data    []byte
		msg     Message
		bodyLen int
	)

	data = valueToBytes(c.Codec, req)

	bodyLen = len(method) + len(data)

	msg = Message(memGet(HeadLen + bodyLen))
	binary.LittleEndian.PutUint32(msg[headerIndexBodyLenBegin:headerIndexBodyLenEnd], uint32(bodyLen))
	binary.LittleEndian.PutUint64(msg[headerIndexSeqBegin:headerIndexSeqEnd], atomic.AddUint64(&c.seq, 1))

	msg[headerIndexCmd] = RPCCmdReq
	msg[headerIndexAsync] = async
	msg[headerIndexMethodLen] = byte(len(method))
	copy(msg[HeadLen:HeadLen+len(method)], method)
	copy(msg[HeadLen+len(method):], data)

	return msg
}

// newClientWithConn factory
func newClientWithConn(conn net.Conn, codec Codec, handler Handler, onStop func() int64) *Client {
	DefaultLogger.Info("[ARPC SVR]\t%v\tConnected", conn.RemoteAddr())

	client := &Client{}
	client.Conn = conn
	client.Reader = handler.WrapReader(conn)
	client.Head = Header(client.head[:])
	client.Codec = codec
	client.Handler = handler
	client.sessionMap = make(map[uint64]*rpcSession)
	client.asyncHandlerMap = make(map[uint64]*asyncHandler)
	client.onStop = onStop

	return client
}

// NewClient factory
func NewClient(dialer func() (net.Conn, error)) (*Client, error) {
	conn, err := dialer()
	if err != nil {
		return nil, err
	}

	DefaultLogger.Info("[ARPC CLI]\t%v\tConnected", conn.RemoteAddr())

	client := &Client{}
	client.Conn = conn
	client.Reader = DefaultHandler.WrapReader(conn)
	client.Head = Header(client.head[:])
	client.Codec = DefaultCodec
	client.Handler = DefaultHandler.Clone()
	client.Dialer = dialer
	client.sessionMap = make(map[uint64]*rpcSession)
	client.asyncHandlerMap = make(map[uint64]*asyncHandler)

	return client, nil
}
