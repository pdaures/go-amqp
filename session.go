package amqp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pdaures/go-amqp/internal/bitmap"
	"github.com/pdaures/go-amqp/internal/debug"
	"github.com/pdaures/go-amqp/internal/encoding"
	"github.com/pdaures/go-amqp/internal/frames"
)

// Default session options
const (
	defaultWindow = 5000
)

// Default link options
const (
	defaultLinkCredit      = 1
	defaultLinkBatching    = false
	defaultLinkBatchMaxAge = 5 * time.Second
)

// Session is an AMQP session.
//
// A session multiplexes Receivers.
type Session struct {
	channel       uint16                       // session's local channel
	remoteChannel uint16                       // session's remote channel, owned by conn.mux
	conn          *conn                        // underlying conn
	rx            chan frames.Frame            // frames destined for this session are sent on this chan by conn.mux
	tx            chan frames.FrameBody        // non-transfer frames to be sent; session must track disposition
	txTransfer    chan *frames.PerformTransfer // transfer frames to be sent; session must track disposition

	// flow control
	incomingWindow uint32
	outgoingWindow uint32
	needFlowCount  uint32

	handleMax uint32

	nextDeliveryID uint32 // atomically accessed sequence for deliveryIDs

	// link management
	linksMu    sync.RWMutex      // used to synchronize link handle allocation
	linksByKey map[linkKey]*link // mapping of name+role link
	handles    *bitmap.Bitmap    // allocated handles

	// used for gracefully closing link
	close     chan struct{}
	closeOnce sync.Once
	done      chan struct{} // part of internal public surface area
	err       error
}

func newSession(c *conn, channel uint16) *Session {
	return &Session{
		conn:           c,
		channel:        channel,
		rx:             make(chan frames.Frame),
		tx:             make(chan frames.FrameBody),
		txTransfer:     make(chan *frames.PerformTransfer),
		incomingWindow: defaultWindow,
		outgoingWindow: defaultWindow,
		handleMax:      math.MaxUint32,
		linksMu:        sync.RWMutex{},
		linksByKey:     make(map[linkKey]*link),
		close:          make(chan struct{}),
		done:           make(chan struct{}),
	}
}

func (s *Session) init(opts *SessionOptions) {
	if opts != nil {
		if opts.IncomingWindow != 0 {
			s.incomingWindow = opts.IncomingWindow
		}
		if opts.MaxLinks != 0 {
			// MaxLinks is the number of total links.
			// handleMax is the max handle ID which starts
			// at zero.  so we decrement by one
			s.handleMax = opts.MaxLinks - 1
		}
		if opts.OutgoingWindow != 0 {
			s.outgoingWindow = opts.OutgoingWindow
		}
	}
	// create handle map after options have been applied
	s.handles = bitmap.New(s.handleMax)
}

// Close gracefully closes the session.
//
// If ctx expires while waiting for servers response, ctx.Err() will be returned.
// The session will continue to wait for the response until the Client is closed.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.close) })
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.err == ErrSessionClosed {
		return nil
	}
	return s.err
}

// txFrame sends a frame to the connWriter.
// it returns an error if the connection has been closed.
func (s *Session) txFrame(p frames.FrameBody, done chan encoding.DeliveryState) error {
	return s.conn.SendFrame(frames.Frame{
		Type:    frames.TypeAMQP,
		Channel: s.channel,
		Body:    p,
		Done:    done,
	})
}

// NewReceiver opens a new receiver link on the session.
// opts: pass nil to accept the default values.
func (s *Session) NewReceiver(ctx context.Context, source string, opts *ReceiverOptions) (*Receiver, error) {
	r := &Receiver{
		batching:    defaultLinkBatching,
		batchMaxAge: defaultLinkBatchMaxAge,
		maxCredit:   defaultLinkCredit,
	}

	l, err := newReceivingLink(source, s, r, opts)
	if err != nil {
		return nil, err
	}
	if err = l.attach(ctx, s); err != nil {
		return nil, err
	}

	r.link = l

	// batching is just extra overhead when maxCredits == 1
	if r.maxCredit == 1 {
		r.batching = false
	}

	// create dispositions channel and start dispositionBatcher if batching enabled
	if r.batching {
		// buffer dispositions chan to prevent disposition sends from blocking
		r.dispositions = make(chan messageDisposition, r.maxCredit)
		go r.dispositionBatcher()
	}

	return r, nil
}

// NewSender opens a new sender link on the session.
// opts: pass nil to accept the default values.
func (s *Session) NewSender(ctx context.Context, target string, opts *SenderOptions) (*Sender, error) {
	l, err := newSendingLink(target, s, opts)
	if err != nil {
		return nil, err
	}
	if err = l.attach(ctx, s); err != nil {
		return nil, err
	}

	return &Sender{link: l}, nil
}

func (s *Session) mux(remoteBegin *frames.PerformBegin) {
	defer func() {
		s.conn.DeleteSession(s)
		if s.err == nil {
			s.err = ErrSessionClosed
		}
		// Signal goroutines waiting on the session.
		close(s.done)
	}()

	var (
		links                     = make(map[uint32]*link)  // mapping of remote handles to links
		handlesByDeliveryID       = make(map[uint32]uint32) // mapping of deliveryIDs to handles
		deliveryIDByHandle        = make(map[uint32]uint32) // mapping of handles to latest deliveryID
		handlesByRemoteDeliveryID = make(map[uint32]uint32) // mapping of remote deliveryID to handles

		settlementByDeliveryID = make(map[uint32]chan encoding.DeliveryState)

		// flow control values
		nextOutgoingID       uint32
		nextIncomingID       = remoteBegin.NextOutgoingID
		remoteIncomingWindow = remoteBegin.IncomingWindow
		remoteOutgoingWindow = remoteBegin.OutgoingWindow
	)

	for {
		txTransfer := s.txTransfer
		// disable txTransfer if flow control windows have been exceeded
		if remoteIncomingWindow == 0 || s.outgoingWindow == 0 {
			debug.Log(1, "TX(Session): Disabling txTransfer - window exceeded. remoteIncomingWindow: %d outgoingWindow:%d",
				remoteIncomingWindow,
				s.outgoingWindow)
			txTransfer = nil
		}

		select {
		// conn has completed, exit
		case <-s.conn.Done:
			s.err = s.conn.Err()
			return

		// session is being closed by user
		case <-s.close:
			_ = s.txFrame(&frames.PerformEnd{}, nil)

			// wait for the ack that the session is closed.
			// we can't exit the mux, which deletes the session,
			// until we receive it.
		EndLoop:
			for {
				select {
				case fr := <-s.rx:
					_, ok := fr.Body.(*frames.PerformEnd)
					if ok {
						break EndLoop
					}
				case <-s.conn.Done:
					s.err = s.conn.Err()
					return
				}
			}
			return

		// incoming frame for link
		case fr := <-s.rx:
			debug.Log(1, "RX(Session): %s", fr.Body)

			switch body := fr.Body.(type) {
			// Disposition frames can reference transfers from more than one
			// link. Send this frame to all of them.
			case *frames.PerformDisposition:
				start := body.First
				end := start
				if body.Last != nil {
					end = *body.Last
				}
				for deliveryID := start; deliveryID <= end; deliveryID++ {
					handles := handlesByDeliveryID
					if body.Role == encoding.RoleSender {
						handles = handlesByRemoteDeliveryID
					}

					handle, ok := handles[deliveryID]
					if !ok {
						debug.Log(2, "role %s: didn't find deliveryID %d in handles map", body.Role, deliveryID)
						continue
					}
					delete(handles, deliveryID)

					if body.Settled && body.Role == encoding.RoleReceiver {
						// check if settlement confirmation was requested, if so
						// confirm by closing channel
						if done, ok := settlementByDeliveryID[deliveryID]; ok {
							delete(settlementByDeliveryID, deliveryID)
							select {
							case done <- body.State:
							default:
							}
							close(done)
						}
					}

					link, ok := links[handle]
					if !ok {
						continue
					}

					s.muxFrameToLink(link, fr.Body)
				}
				continue
			case *frames.PerformFlow:
				if body.NextIncomingID == nil {
					// This is a protocol error:
					//       "[...] MUST be set if the peer has received
					//        the begin frame for the session"
					_ = s.txFrame(&frames.PerformEnd{
						Error: &Error{
							Condition:   ErrorNotAllowed,
							Description: "next-incoming-id not set after session established",
						},
					}, nil)
					s.err = errors.New("protocol error: received flow without next-incoming-id after session established")
					return
				}

				// "When the endpoint receives a flow frame from its peer,
				// it MUST update the next-incoming-id directly from the
				// next-outgoing-id of the frame, and it MUST update the
				// remote-outgoing-window directly from the outgoing-window
				// of the frame."
				nextIncomingID = body.NextOutgoingID
				remoteOutgoingWindow = body.OutgoingWindow

				// "The remote-incoming-window is computed as follows:
				//
				// next-incoming-id(flow) + incoming-window(flow) - next-outgoing-id(endpoint)
				//
				// If the next-incoming-id field of the flow frame is not set, then remote-incoming-window is computed as follows:
				//
				// initial-outgoing-id(endpoint) + incoming-window(flow) - next-outgoing-id(endpoint)"
				remoteIncomingWindow = body.IncomingWindow - nextOutgoingID
				remoteIncomingWindow += *body.NextIncomingID
				debug.Log(3, "RX(Session) Flow - remoteOutgoingWindow: %d remoteIncomingWindow: %d nextOutgoingID: %d", remoteOutgoingWindow, remoteIncomingWindow, nextOutgoingID)

				// Send to link if handle is set
				if body.Handle != nil {
					link, ok := links[*body.Handle]
					if !ok {
						continue
					}

					s.muxFrameToLink(link, fr.Body)
					continue
				}

				if body.Echo {
					niID := nextIncomingID
					resp := &frames.PerformFlow{
						NextIncomingID: &niID,
						IncomingWindow: s.incomingWindow,
						NextOutgoingID: nextOutgoingID,
						OutgoingWindow: s.outgoingWindow,
					}
					debug.Log(1, "TX (session.mux): %s", resp)
					_ = s.txFrame(resp, nil)
				}

			case *frames.PerformAttach:
				// On Attach response link should be looked up by name, then added
				// to the links map with the remote's handle contained in this
				// attach frame.
				//
				// Note body.Role is the remote peer's role, we reverse for the local key.
				s.linksMu.RLock()
				link, linkOk := s.linksByKey[linkKey{name: body.Name, role: !body.Role}]
				s.linksMu.RUnlock()
				if !linkOk {
					s.err = fmt.Errorf("protocol error: received mismatched attach frame %+v", body)
					return
				}

				link.RemoteHandle = body.Handle
				links[link.RemoteHandle] = link

				s.muxFrameToLink(link, fr.Body)

			case *frames.PerformTransfer:
				s.needFlowCount++
				// "Upon receiving a transfer, the receiving endpoint will
				// increment the next-incoming-id to match the implicit
				// transfer-id of the incoming transfer plus one, as well
				// as decrementing the remote-outgoing-window, and MAY
				// (depending on policy) decrement its incoming-window."
				nextIncomingID++
				// don't loop to intmax
				if remoteOutgoingWindow > 0 {
					remoteOutgoingWindow--
				}
				link, ok := links[body.Handle]
				if !ok {
					// TODO: per section 2.8.17 I think this should return an error
					continue
				}

				select {
				case <-s.conn.Done:
				case link.RX <- fr.Body:
				}

				// if this message is received unsettled and link rcv-settle-mode == second, add to handlesByRemoteDeliveryID
				if !body.Settled && body.DeliveryID != nil && link.ReceiverSettleMode != nil && *link.ReceiverSettleMode == ModeSecond {
					debug.Log(1, "TX(Session): adding handle to handlesByRemoteDeliveryID. delivery ID: %d", *body.DeliveryID)
					handlesByRemoteDeliveryID[*body.DeliveryID] = body.Handle
				}

				// Update peer's outgoing window if half has been consumed.
				if s.needFlowCount >= s.incomingWindow/2 {
					debug.Log(3, "TX(Session %d) Flow s.needFlowCount(%d) >= s.incomingWindow(%d)/2\n", s.channel, s.needFlowCount, s.incomingWindow)
					s.needFlowCount = 0
					nID := nextIncomingID
					flow := &frames.PerformFlow{
						NextIncomingID: &nID,
						IncomingWindow: s.incomingWindow,
						NextOutgoingID: nextOutgoingID,
						OutgoingWindow: s.outgoingWindow,
					}
					debug.Log(1, "TX(Session): %s", flow)
					_ = s.txFrame(flow, nil)
				}

			case *frames.PerformDetach:
				link, ok := links[body.Handle]
				if !ok {
					// TODO: per section 2.8.17 I think this should return an error
					continue
				}
				s.muxFrameToLink(link, fr.Body)

				// we received a detach frame and sent it to the link.
				// this was either the response to a client-side initiated
				// detach or our peer detached us. either way, now that
				// the link has processed the frame it's detached so we
				// are safe to clean up its state.
				delete(links, link.RemoteHandle)
				delete(deliveryIDByHandle, link.Handle)

			case *frames.PerformEnd:
				_ = s.txFrame(&frames.PerformEnd{}, nil)
				s.err = fmt.Errorf("session ended by server: %s", body.Error)
				return

			default:
				// TODO: evaluate
				debug.Log(1, "session mux: unexpected frame: %s\n", body)
			}

		case fr := <-txTransfer:

			// record current delivery ID
			var deliveryID uint32
			if fr.DeliveryID != nil {
				deliveryID = *fr.DeliveryID
				deliveryIDByHandle[fr.Handle] = deliveryID

				// add to handleByDeliveryID if not sender-settled
				if !fr.Settled {
					handlesByDeliveryID[deliveryID] = fr.Handle
				}
			} else {
				// if fr.DeliveryID is nil it must have been added
				// to deliveryIDByHandle already
				deliveryID = deliveryIDByHandle[fr.Handle]
			}

			// frame has been sender-settled, remove from map
			if fr.Settled {
				delete(handlesByDeliveryID, deliveryID)
			}

			// if not settled, add done chan to map
			// and clear from frame so conn doesn't close it.
			if !fr.Settled && fr.Done != nil {
				settlementByDeliveryID[deliveryID] = fr.Done
				fr.Done = nil
			}

			debug.Log(2, "TX(Session) - txtransfer: %s", fr)
			_ = s.txFrame(fr, fr.Done)

			// "Upon sending a transfer, the sending endpoint will increment
			// its next-outgoing-id, decrement its remote-incoming-window,
			// and MAY (depending on policy) decrement its outgoing-window."
			nextOutgoingID++
			// don't decrement if we're at 0 or we could loop to int max
			if remoteIncomingWindow != 0 {
				remoteIncomingWindow--
			}

		case fr := <-s.tx:
			switch fr := fr.(type) {
			case *frames.PerformFlow:
				niID := nextIncomingID
				fr.NextIncomingID = &niID
				fr.IncomingWindow = s.incomingWindow
				fr.NextOutgoingID = nextOutgoingID
				fr.OutgoingWindow = s.outgoingWindow
				debug.Log(1, "TX(Session) - tx: %s", fr)
				_ = s.txFrame(fr, nil)
			case *frames.PerformTransfer:
				panic("transfer frames must use txTransfer")
			default:
				debug.Log(1, "TX(Session) - default: %s", fr)
				_ = s.txFrame(fr, nil)
			}
		}
	}
}

func (s *Session) allocateHandle(l *link) error {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	// Check if link name already exists, if so then an error should be returned
	existing := s.linksByKey[l.Key]
	if existing != nil {
		return fmt.Errorf("link with name '%v' already exists", l.Key.name)
	}

	next, ok := s.handles.Next()
	if !ok {
		// handle numbers are zero-based, report the actual count
		return fmt.Errorf("reached session handle max (%d)", s.handleMax+1)
	}

	l.Handle = next         // allocate handle to the link
	s.linksByKey[l.Key] = l // add to mapping

	return nil
}

func (s *Session) deallocateHandle(l *link) {
	s.linksMu.Lock()
	defer s.linksMu.Unlock()

	delete(s.linksByKey, l.Key)
	s.handles.Remove(l.Handle)
	close(l.RX)
}

func (s *Session) muxFrameToLink(l *link, fr frames.FrameBody) {
	select {
	case l.RX <- fr:
		// frame successfully sent to link
	case <-l.Detached:
		// link is closed
		// this should be impossible to hit as the link has been removed from the session once Detached is closed
	case <-s.conn.Done:
		// conn is closed
	}
}
