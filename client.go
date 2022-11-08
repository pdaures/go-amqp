package amqp

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pdaures/go-amqp/internal/debug"
	"github.com/pdaures/go-amqp/internal/encoding"
	"github.com/pdaures/go-amqp/internal/frames"
)

// Client is an AMQP client connection.
type Client struct {
	conn *conn
}

// Dial connects to an AMQP server.
//
// If the addr includes a scheme, it must be "amqp", "amqps", or "amqp+ssl".
// If no port is provided, 5672 will be used for "amqp" and 5671 for "amqps" or "amqp+ssl".
//
// If username and password information is not empty it's used as SASL PLAIN
// credentials, equal to passing ConnSASLPlain option.
//
// opts: pass nil to accept the default values.
func Dial(addr string, opts *ConnOptions) (*Client, error) {
	c, err := dialConn(addr, opts)
	if err != nil {
		return nil, err
	}
	err = c.Start()
	if err != nil {
		return nil, err
	}
	return &Client{conn: c}, nil
}

// New establishes an AMQP client connection over conn.
// opts: pass nil to accept the default values.
func New(conn net.Conn, opts *ConnOptions) (*Client, error) {
	c, err := newConn(conn, opts)
	if err != nil {
		return nil, err
	}
	err = c.Start()
	if err != nil {
		return nil, err
	}
	return &Client{conn: c}, nil
}

// Close disconnects the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// NewSession opens a new AMQP session to the server.
// Returns ErrConnClosed if the underlying connection has been closed.
// opts: pass nil to accept the default values.
func (c *Client) NewSession(ctx context.Context, opts *SessionOptions) (*Session, error) {
	s, err := c.conn.NewSession()
	if err != nil {
		return nil, err
	}
	s.init(opts)

	// send Begin to server
	begin := &frames.PerformBegin{
		NextOutgoingID: 0,
		IncomingWindow: s.incomingWindow,
		OutgoingWindow: s.outgoingWindow,
		HandleMax:      s.handleMax,
	}
	debug.Log(1, "TX (NewSession): %s", begin)

	// we use send to have positive confirmation on transmission
	send := make(chan encoding.DeliveryState)
	_ = s.txFrame(begin, send)

	// wait for response
	var fr frames.Frame
	select {
	case <-ctx.Done():
		select {
		case <-send:
			// begin was written to the network.  assume it was
			// received and that the ctx was too short to wait for
			// the ack. in this case we must send an end before we
			// can delete the session
			go func() {
				_ = s.txFrame(&frames.PerformEnd{}, nil)
				select {
				case <-c.conn.Done:
					// conn has terminated, no need to delete the session
				case <-time.After(5 * time.Second):
					debug.Log(3, "NewSession clean-up timed out waiting for PerformEnd ack")
				case <-s.rx:
					// received ack that session was closed, safe to delete session
					c.conn.DeleteSession(s)
				}
			}()
		default:
			// begin wasn't written to the network, so delete session
			c.conn.DeleteSession(s)
		}
		return nil, ctx.Err()
	case <-c.conn.Done:
		return nil, c.conn.Err()
	case fr = <-s.rx:
		// received ack that session was created
	}
	debug.Log(1, "RX (NewSession): %s", fr.Body)

	begin, ok := fr.Body.(*frames.PerformBegin)
	if !ok {
		// this codepath is hard to hit (impossible?).  if the response isn't a PerformBegin and we've not
		// yet seen the remote channel number, the default clause in conn.mux will protect us from that.
		// if we have seen the remote channel number then it's likely the session.mux for that channel will
		// either swallow the frame or blow up in some other way, both causing this call to hang.
		// deallocate session on error.  we can't call
		// s.Close() as the session mux hasn't started yet.
		c.conn.DeleteSession(s)
		return nil, fmt.Errorf("unexpected begin response: %+v", fr.Body)
	}

	// start Session multiplexor
	go s.mux(begin)

	return s, nil
}

// SessionOption contains the optional settings for configuring an AMQP session.
type SessionOptions struct {
	// IncomingWindow sets the maximum number of unacknowledged
	// transfer frames the server can send.
	IncomingWindow uint32

	// OutgoingWindow sets the maximum number of unacknowledged
	// transfer frames the client can send.
	OutgoingWindow uint32

	// MaxLinks sets the maximum number of links (Senders/Receivers)
	// allowed on the session.
	//
	// Minimum: 1.
	// Default: 4294967295.
	MaxLinks uint32
}

// linkKey uniquely identifies a link on a connection by name and direction.
//
// A link can be identified uniquely by the ordered tuple
//
//	(source-container-id, target-container-id, name)
//
// On a single connection the container ID pairs can be abbreviated
// to a boolean flag indicating the direction of the link.
type linkKey struct {
	name string
	role encoding.Role // Local role: sender/receiver
}

const maxTransferFrameHeader = 66 // determined by calcMaxTransferFrameHeader
