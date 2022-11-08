package amqp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/pdaures/go-amqp/internal/encoding"
	"github.com/pdaures/go-amqp/internal/frames"
	"github.com/pdaures/go-amqp/internal/mocks"
	"github.com/stretchr/testify/require"
)

type mockDialer struct {
	resp func(frames.FrameBody) ([]byte, error)
}

func (m mockDialer) NetDialerDial(c *conn, host, port string) error {
	c.net = mocks.NewNetConn(m.resp)
	return nil
}

func (mockDialer) TLSDialWithDialer(c *conn, host, port string) error {
	panic("nyi")
}

func TestClientDial(t *testing.T) {
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	client, err := Dial("amqp://localhost", &ConnOptions{dialer: mockDialer{resp: responder}})
	require.NoError(t, err)
	require.NotNil(t, client)
	// error case
	responder = func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return nil, errors.New("mock read failed")
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	client, err = Dial("amqp://localhost", &ConnOptions{dialer: mockDialer{resp: responder}})
	require.Error(t, err)
	require.Nil(t, client)
}

func TestClientClose(t *testing.T) {
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	client, err := Dial("amqp://localhost", &ConnOptions{dialer: mockDialer{resp: responder}})
	require.NoError(t, err)
	require.NotNil(t, client)
	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
}

func TestSessionOptions(t *testing.T) {
	const (
		// MaxInt added in Go 1.17, this is copied from there
		intSize = 32 << (^uint(0) >> 63) // 32 or 64
		MaxInt  = 1<<(intSize-1) - 1
	)
	tests := []struct {
		label  string
		opt    SessionOptions
		verify func(t *testing.T, s *Session)
	}{
		{
			label: "SessionIncomingWindow",
			opt: SessionOptions{
				IncomingWindow: 5000,
			},
			verify: func(t *testing.T, s *Session) {
				if s.incomingWindow != 5000 {
					t.Errorf("unexpected incoming window %d", s.incomingWindow)
				}
			},
		},
		{
			label: "SessionOutgoingWindow",
			opt: SessionOptions{
				OutgoingWindow: 6000,
			},
			verify: func(t *testing.T, s *Session) {
				if s.outgoingWindow != 6000 {
					t.Errorf("unexpected outgoing window %d", s.outgoingWindow)
				}
			},
		},
		{
			label: "SessionMaxLinks",
			opt: SessionOptions{
				MaxLinks: 4096,
			},
			verify: func(t *testing.T, s *Session) {
				if s.handleMax != 4096-1 {
					t.Errorf("unexpected max links %d", s.handleMax)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			session := newSession(nil, 0)
			session.init(&tt.opt)
		})
	}
}

func TestClientNewSession(t *testing.T) {
	const channelNum = 0
	const incomingWindow = 5000
	const outgoingWindow = 6000
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch tt := req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			if tt.RemoteChannel != nil {
				return nil, errors.New("expected nil remote channel")
			}
			if tt.IncomingWindow != incomingWindow {
				return nil, fmt.Errorf("unexpected incoming window %d", tt.IncomingWindow)
			}
			if tt.OutgoingWindow != outgoingWindow {
				return nil, fmt.Errorf("unexpected incoming window %d", tt.OutgoingWindow)
			}
			return mocks.PerformBegin(channelNum)
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, &SessionOptions{
		IncomingWindow: incomingWindow,
		OutgoingWindow: outgoingWindow,
	})
	cancel()
	require.NoError(t, err)
	require.NotNil(t, session)
	require.Equal(t, uint16(channelNum), session.channel)
	require.NoError(t, client.Close())
	// creating a session after the connection has been closed returns nothing
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
	session, err = client.NewSession(ctx, nil)
	cancel()
	var connErr *ConnectionError
	if !errors.As(err, &connErr) {
		t.Fatalf("unexpected error type %T", err)
	}
	require.Equal(t, "amqp: connection closed", connErr.Error())
	require.Nil(t, session)
}

func TestClientMultipleSessions(t *testing.T) {
	channelNum := uint16(0)
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			b, err := mocks.PerformBegin(channelNum)
			channelNum++
			return b, err
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// first session
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session1, err := client.NewSession(ctx, nil)
	cancel()
	require.NoError(t, err)
	require.NotNil(t, session1)
	require.Equal(t, channelNum-1, session1.channel)
	// second session
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
	session2, err := client.NewSession(ctx, nil)
	cancel()
	require.NoError(t, err)
	require.NotNil(t, session2)
	require.Equal(t, channelNum-1, session2.channel)
	require.NoError(t, client.Close())
}

func TestClientTooManySessions(t *testing.T) {
	channelNum := uint16(0)
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			// return small number of max channels
			return mocks.EncodeFrame(mocks.FrameAMQP, 0, &frames.PerformOpen{
				ChannelMax:   1,
				ContainerID:  "test",
				IdleTimeout:  time.Minute,
				MaxFrameSize: 4294967295,
			})
		case *frames.PerformBegin:
			b, err := mocks.PerformBegin(channelNum)
			channelNum++
			return b, err
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	for i := uint16(0); i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		session, err := client.NewSession(ctx, nil)
		cancel()
		if i < 2 {
			require.NoError(t, err)
			require.NotNil(t, session)
		} else {
			// third channel should fail
			require.Error(t, err)
			require.Nil(t, session)
		}
	}
	require.NoError(t, client.Close())
}

func TestClientNewSessionMissingRemoteChannel(t *testing.T) {
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			// return begin with nil RemoteChannel
			return mocks.EncodeFrame(mocks.FrameAMQP, 0, &frames.PerformBegin{
				NextOutgoingID: 1,
				IncomingWindow: 5000,
				OutgoingWindow: 1000,
				HandleMax:      math.MaxInt16,
			})
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, &SessionOptions{
		MaxLinks: 1,
	})
	cancel()
	require.Error(t, err)
	require.Nil(t, session)
	require.Error(t, client.Close())
}

func TestClientNewSessionInvalidInitialResponse(t *testing.T) {
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			// respond with the wrong frame type
			return mocks.PerformOpen("bad")
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	require.Error(t, err)
	require.Nil(t, session)
}

func TestClientNewSessionInvalidSecondResponseSameChannel(t *testing.T) {
	t.Skip("test hangs due to session mux eating unexpected frames")
	firstChan := true
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			if firstChan {
				firstChan = false
				return mocks.PerformBegin(0)
			}
			// respond with the wrong frame type
			return mocks.PerformOpen("bad")
		case *frames.PerformEnd:
			return mocks.PerformEnd(0, nil)
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// fisrt session succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	require.NoError(t, err)
	require.NotNil(t, session)
	// second session fails
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
	session, err = client.NewSession(ctx, nil)
	cancel()
	require.Error(t, err)
	require.Nil(t, session)
	require.NoError(t, client.Close())
}

func TestClientNewSessionInvalidSecondResponseDifferentChannel(t *testing.T) {
	firstChan := true
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			if firstChan {
				firstChan = false
				return mocks.PerformBegin(0)
			}
			// respond with the wrong frame type
			// note that it has to be for the next channel
			return mocks.PerformDisposition(encoding.RoleSender, 1, 0, nil, nil)
		case *frames.PerformEnd:
			return mocks.PerformEnd(0, nil)
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// fisrt session succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	require.NoError(t, err)
	require.NotNil(t, session)
	// second session fails
	ctx, cancel = context.WithTimeout(context.Background(), 1*time.Second)
	session, err = client.NewSession(ctx, nil)
	cancel()
	require.Error(t, err)
	require.Nil(t, session)
	require.Error(t, client.Close())
}

func TestNewSessionTimedOut(t *testing.T) {
	endAck := make(chan struct{})
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			// swallow the frame so NewSession never gets an ack
			return nil, nil
		case *frames.PerformEnd:
			close(endAck)
			return mocks.PerformEnd(0, nil)
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// fisrt session succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, session)

	select {
	case <-time.After(time.Second):
		t.Fatal("didn't receive end ack")
	case <-endAck:
		// expected
	}
}

func TestNewSessionWriteError(t *testing.T) {
	endAck := make(chan struct{})
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			return nil, errors.New("write error")
		case *frames.PerformEnd:
			close(endAck)
			return mocks.PerformEnd(0, nil)
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// fisrt session succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	var connErr *ConnectionError
	require.ErrorAs(t, err, &connErr)
	require.Equal(t, "write error", connErr.Error())
	require.Nil(t, session)

	select {
	case <-time.After(time.Second):
		// expected
	case <-endAck:
		t.Fatal("unexpected ack")
	}
}

func TestNewSessionTimedOutAckTimedOut(t *testing.T) {
	endAck := make(chan struct{})
	responder := func(req frames.FrameBody) ([]byte, error) {
		switch req.(type) {
		case *mocks.AMQPProto:
			return []byte{'A', 'M', 'Q', 'P', 0, 1, 0, 0}, nil
		case *frames.PerformOpen:
			return mocks.PerformOpen("container")
		case *frames.PerformBegin:
			// swallow the frame so NewSession never gets an ack
			return nil, nil
		case *frames.PerformEnd:
			close(endAck)
			// swallow the frame so the closing goroutine never gets an ack
			return nil, nil
		default:
			return nil, fmt.Errorf("unhandled frame %T", req)
		}
	}
	netConn := mocks.NewNetConn(responder)

	client, err := New(netConn, nil)
	require.NoError(t, err)
	// fisrt session succeeds
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	session, err := client.NewSession(ctx, nil)
	cancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, session)

	select {
	case <-time.After(time.Second):
		t.Fatal("didn't receive end ack")
	case <-endAck:
		// expected
	}
}
