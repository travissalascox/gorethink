package gorethink

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	p "github.com/dancannon/gorethink/ql2"
)

type connRequest struct {
	Active int32

	Query    Query
	Options  map[string]interface{}
	Response chan connResponse
}

type connResponse struct {
	Response *Response
	Error    error
}

type Response struct {
	Token     int64
	Type      p.Response_ResponseType `json:"t"`
	Responses []interface{}           `json:"r"`
	Backtrace []interface{}           `json:"b"`
	Profile   interface{}             `json:"p"`
}

type Conn interface {
	SendQuery(s *Session, q *p.Query, t Term, opts map[string]interface{}) (*Cursor, error)
	ReadResponse(s *Session, token int64) (*Response, error)
	Close() error
}

// connection is a connection to a rethinkdb database
type Connection struct {
	sync.Mutex
	conn    net.Conn
	session *Session
	pool    ConnectionPool

	token       int64
	closed      bool
	outstanding int64
	cursors     map[int64]*Cursor
	requests    map[int64]connRequest
}

// Dial closes the previous connection and attempts to connect again.
func Dial(s *Session) (net.Conn, error) {
	conn, err := net.Dial("tcp", s.Opts.Address)
	if err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Send the protocol version to the server as a 4-byte little-endian-encoded integer
	if err := binary.Write(conn, binary.LittleEndian, p.VersionDummy_V0_3); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Send the length of the auth key to the server as a 4-byte little-endian-encoded integer
	if err := binary.Write(conn, binary.LittleEndian, uint32(len(s.Opts.AuthKey))); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Send the auth key as an ASCII string
	// If there is no auth key, skip this step
	if s.Opts.AuthKey != "" {
		if _, err := io.WriteString(conn, s.Opts.AuthKey); err != nil {
			return nil, RqlConnectionError{err.Error()}
		}
	}

	// Send the protocol type as a 4-byte little-endian-encoded integer
	if err := binary.Write(conn, binary.LittleEndian, p.VersionDummy_JSON); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// read server response to authorization key (terminated by NUL)
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\x00')
	if err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("Unexpected EOF: %s", string(line))
		}
		return nil, RqlDriverError{err.Error()}
	}
	// convert to string and remove trailing NUL byte
	response := string(line[:len(line)-1])
	if response != "SUCCESS" {
		// we failed authorization or something else terrible happened
		return nil, RqlDriverError{fmt.Sprintf("Server dropped connection with message: \"%s\"", response)}
	}

	return conn, nil
}

func newConnection(s *Session, c net.Conn, p ConnectionPool) *Connection {
	conn := &Connection{
		conn:    c,
		session: s,
		pool:    p,

		cursors:  make(map[int64]*Cursor),
		requests: make(map[int64]connRequest),
	}

	go conn.readLoop()

	return conn
}

func (c *Connection) StartQuery(t Term, opts map[string]interface{}) (*Cursor, error) {
	token := c.nextToken()

	// Build global options
	globalOpts := map[string]interface{}{}
	for k, v := range opts {
		globalOpts[k] = Expr(v).build()
	}

	// If no DB option was set default to the value set in the connection
	if _, ok := opts["db"]; !ok {
		globalOpts["db"] = Db(c.session.Opts.Database).build()
	}

	// Construct query
	q := Query{
		Type:       p.Query_START,
		Token:      token,
		Term:       &t,
		GlobalOpts: globalOpts,
	}

	_, cursor, err := c.SendQuery(q, opts)
	return cursor, err
}

func (c *Connection) ContinueQuery(token int64) error {
	q := Query{
		Type:  p.Query_CONTINUE,
		Token: token,
	}

	_, _, err := c.SendQuery(q, map[string]interface{}{})
	return err
}

func (c *Connection) StopQuery(token int64) error {
	q := Query{
		Type:  p.Query_STOP,
		Token: token,
	}

	_, _, err := c.SendQuery(q, map[string]interface{}{})
	return err
}

func (c *Connection) NoReplyWait() error {
	q := Query{
		Type:  p.Query_NOREPLY_WAIT,
		Token: c.nextToken(),
	}

	_, _, err := c.SendQuery(q, map[string]interface{}{})
	return err
}

func (c *Connection) SendQuery(q Query, opts map[string]interface{}) (*Response, *Cursor, error) {
	request := connRequest{
		Query:   q,
		Options: opts,
	}
	request.Response = make(chan connResponse, 1)
	atomic.AddInt64(&c.outstanding, 1)
	atomic.StoreInt32(&request.Active, 1)

	c.Lock()
	c.requests[q.Token] = request
	c.Unlock()

	c.sendQuery(request)

	if noreply, ok := opts["noreply"]; ok && noreply.(bool) {
		return nil, nil, nil
	}

	reply := <-request.Response
	if reply.Error != nil {
		return nil, nil, reply.Error
	}

	return c.processResponse(request, reply.Response)
}

func (c *Connection) sendQuery(request connRequest) error {
	c.Lock()
	closed := c.closed
	c.Unlock()

	c.session.Lock()
	timeout := c.session.Opts.Timeout
	c.session.Unlock()

	if closed {
		return ErrConnectionClosed
	}
	// Build query
	b, err := json.Marshal(request.Query.build())
	if err != nil {
		return RqlDriverError{"Error building query"}
	}

	// Set timeout
	if timeout == 0 {
		c.conn.SetDeadline(time.Time{})
	} else {
		c.conn.SetDeadline(time.Now().Add(timeout))
	}

	// Send a unique 8-byte token
	if err = binary.Write(c.conn, binary.LittleEndian, request.Query.Token); err != nil {
		return RqlConnectionError{err.Error()}
	}

	// Send the length of the JSON-encoded query as a 4-byte
	// little-endian-encoded integer.
	if err = binary.Write(c.conn, binary.LittleEndian, uint32(len(b))); err != nil {
		return RqlConnectionError{err.Error()}
	}

	// Send the JSON encoding of the query itself.
	if err = binary.Write(c.conn, binary.BigEndian, b); err != nil {
		return RqlConnectionError{err.Error()}
	}

	return nil
}

func (c *Connection) GetConn() (*Connection, error) {
	return c, nil
}

// Close closes the underlying net.Conn. It also removes the connection
// from the connection pool
func (c *Connection) Close() error {
	c.Lock()
	closed := c.closed
	c.Unlock()

	if !closed {
		err := c.conn.Close()

		c.Lock()
		c.closed = true
		c.Unlock()

		return err
	}

	return nil
}

// Return returns the connection to the connection pool
func (c *Connection) Return() error {
	c.Lock()
	closed := c.closed
	c.Unlock()

	if !closed {
		err := c.conn.Close()

		c.Lock()
		c.closed = true
		c.Unlock()

		return err
	}

	return nil
}

// getToken generates the next query token, used to number requests and match
// responses with requests.
func (c *Connection) nextToken() int64 {
	return atomic.AddInt64(&c.token, 1)
}

func (c *Connection) processResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	switch response.Type {
	case p.Response_CLIENT_ERROR:
		return c.processErrorResponse(request, response, RqlClientError{rqlResponseError{response, request.Query.Term}})
	case p.Response_COMPILE_ERROR:
		return c.processErrorResponse(request, response, RqlCompileError{rqlResponseError{response, request.Query.Term}})
	case p.Response_RUNTIME_ERROR:
		return c.processErrorResponse(request, response, RqlRuntimeError{rqlResponseError{response, request.Query.Term}})
	case p.Response_SUCCESS_ATOM:
		return c.processAtomResponse(request, response)
	case p.Response_SUCCESS_FEED:
		return c.processFeedResponse(request, response)
	case p.Response_SUCCESS_PARTIAL:
		return c.processPartialResponse(request, response)
	case p.Response_SUCCESS_SEQUENCE:
		return c.processSequenceResponse(request, response)
	case p.Response_WAIT_COMPLETE:
		return c.processWaitResponse(request, response)
	default:
		return nil, nil, RqlDriverError{"Unexpected response type"}
	}
}

func (c *Connection) processErrorResponse(request connRequest, response *Response, err error) (*Response, *Cursor, error) {
	// c.Close()

	c.Lock()
	cursor := c.cursors[response.Token]

	// delete(c.requests, response.Token)
	// delete(c.cursors, response.Token)
	c.Unlock()

	return response, cursor, err
}

func (c *Connection) processAtomResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	// c.Close()

	// Create cursor
	var value []interface{}
	if len(response.Responses) == 0 {
		value = []interface{}{}
	} else {
		v, err := recursivelyConvertPseudotype(response.Responses[0], request.Options)
		if err != nil {
			return nil, nil, err
		}

		if sv, ok := v.([]interface{}); ok {
			value = sv
		} else if v == nil {
			value = []interface{}{nil}
		} else {
			value = []interface{}{v}
		}
	}

	cursor := newCursor(c.session, c, response.Token, request.Query.Term, request.Options)
	cursor.profile = response.Profile
	cursor.buffer = value
	cursor.finished = true

	c.Lock()
	// delete(c.requests, response.Token)
	c.Unlock()

	return response, cursor, nil
}

func (c *Connection) processFeedResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	var cursor *Cursor
	if _, ok := c.cursors[response.Token]; !ok {
		// Create a new cursor if needed
		cursor = newCursor(c.session, c, response.Token, request.Query.Term, request.Options)
		cursor.profile = response.Profile
		c.cursors[response.Token] = cursor
	} else {
		cursor = c.cursors[response.Token]
	}

	c.Lock()
	// delete(c.requests, response.Token)
	c.Unlock()

	cursor.extend(response)

	return response, cursor, nil
}

func (c *Connection) processPartialResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	c.Lock()
	cursor, ok := c.cursors[response.Token]
	c.Unlock()

	if !ok {
		// Create a new cursor if needed
		cursor = newCursor(c.session, c, response.Token, request.Query.Term, request.Options)
		cursor.profile = response.Profile

		c.Lock()
		c.cursors[response.Token] = cursor
		c.Unlock()
	}

	c.Lock()
	// delete(c.requests, response.Token)
	c.Unlock()

	cursor.extend(response)
	return response, cursor, nil
}

func (c *Connection) processSequenceResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	// c.Close()

	c.Lock()
	cursor, ok := c.cursors[response.Token]
	c.Unlock()

	if !ok {
		// Create a new cursor if needed
		cursor = newCursor(c.session, c, response.Token, request.Query.Term, request.Options)
		cursor.profile = response.Profile

		c.Lock()
		c.cursors[response.Token] = cursor
		c.Unlock()
	}

	c.Lock()
	// delete(c.requests, response.Token)
	// delete(c.cursors, response.Token)
	c.Unlock()

	cursor.extend(response)

	return response, cursor, nil
}

func (c *Connection) processWaitResponse(request connRequest, response *Response) (*Response, *Cursor, error) {
	// c.Close()

	c.Lock()
	// delete(c.requests, response.Token)
	// delete(c.cursors, response.Token)
	c.Unlock()

	return response, nil, nil
}

func (c *Connection) readLoop() {
	var response *Response
	var err error

	for {
		response, err = c.read()
		if err != nil {
			break
		}

		// Process response
		c.Lock()
		request, ok := c.requests[response.Token]
		c.Unlock()

		// If the cached request could not be found skip processing
		if !ok {
			continue
		}

		// If the cached request is not active skip processing
		if !atomic.CompareAndSwapInt32(&request.Active, 1, 0) {
			continue
		}
		atomic.AddInt64(&c.outstanding, -1)
		request.Response <- connResponse{response, err}
	}

	c.Close()

	c.Lock()
	requests := c.requests
	c.Unlock()
	for _, request := range requests {
		if atomic.LoadInt32(&request.Active) == 1 {
			request.Response <- connResponse{
				Response: response,
				Error:    err,
			}
		}
	}

	c.pool.HandleError(c, err, true)
}

func (c *Connection) read() (*Response, error) {
	// Read the 8-byte token of the query the response corresponds to.
	var responseToken int64
	if err := binary.Read(c.conn, binary.LittleEndian, &responseToken); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Read the length of the JSON-encoded response as a 4-byte
	// little-endian-encoded integer.
	var messageLength uint32
	if err := binary.Read(c.conn, binary.LittleEndian, &messageLength); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Read the JSON encoding of the Response itself.
	b := make([]byte, messageLength)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return nil, RqlConnectionError{err.Error()}
	}

	// Decode the response
	var response = new(Response)
	if err := json.Unmarshal(b, response); err != nil {
		return nil, RqlDriverError{err.Error()}
	}
	response.Token = responseToken

	return response, nil
}
