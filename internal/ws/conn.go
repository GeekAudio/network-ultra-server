package ws

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Conn wraps a nhooyr WebSocket with a writeQueue + write goroutine so multiple
// senders (forwarder + control handlers) don't race on the underlying conn.
//
// Backpressure policy:
//   - Control frames (text) MUST be delivered: blocking enqueue with short
//     timeout; on persistent failure the conn is closed (the peer is dead).
//   - Audio frames (binary) MAY be dropped: when the queue is full we drop
//     the OLDEST queued audio frame to make room, so a slow long-RTT receiver
//     doesn't bring the whole connection down. This is what lets sessions
//     stay alive over 500 ms+ RTT links where TCP momentarily stalls.
type Conn struct {
	c   *websocket.Conn
	log *slog.Logger

	writeCh chan outgoing
	doneCh  chan struct{}
	wg      sync.WaitGroup

	closeOnce sync.Once
}

type outgoing struct {
	mt   websocket.MessageType
	data []byte
}

func newConn(c *websocket.Conn, log *slog.Logger) *Conn {
	cn := &Conn{
		c:       c,
		log:     log,
		writeCh: make(chan outgoing, connWriteQueueDepth),
		doneCh:  make(chan struct{}),
	}
	cn.wg.Add(1)
	go cn.writeLoop()
	return cn
}

// SendFunc returns a thread-safe send function suitable for room.Peer.AttachSender.
func (cn *Conn) SendFunc() func([]byte, bool) error {
	return func(p []byte, isBinary bool) error {
		mt := websocket.MessageText
		if isBinary {
			mt = websocket.MessageBinary
		}
		return cn.write(mt, p)
	}
}

func (cn *Conn) read(ctx context.Context) (websocket.MessageType, []byte, error) {
	return cn.c.Read(ctx)
}

func (cn *Conn) write(mt websocket.MessageType, data []byte) error {
	// Audio (binary) is droppable; control (text) is not. The split exists so
	// a transient long-RTT stall on the receive side can't kill the session
	// as long as control plane is still flowing.
	if mt == websocket.MessageBinary {
		return cn.writeAudio(data)
	}
	return cn.writeControl(mt, data)
}

// writeControl enqueues a text/control frame. Blocks briefly when the queue is
// momentarily full (long RTT spikes happen) and only closes the conn if the
// queue is *still* full after the timeout — at which point the peer is
// genuinely dead.
func (cn *Conn) writeControl(mt websocket.MessageType, data []byte) error {
	select {
	case cn.writeCh <- outgoing{mt: mt, data: data}:
		return nil
	case <-cn.doneCh:
		return errors.New("conn closed")
	default:
	}
	// Queue momentarily full. Wait up to writeTimeout for it to drain.
	timer := time.NewTimer(writeTimeout)
	defer timer.Stop()
	select {
	case cn.writeCh <- outgoing{mt: mt, data: data}:
		return nil
	case <-cn.doneCh:
		return errors.New("conn closed")
	case <-timer.C:
		cn.close()
		return errors.New("control write queue stuck")
	}
}

// writeAudio enqueues a binary audio frame. Never blocks: if the queue is
// full it drops the OLDEST queued audio frame and inserts the new one. This
// keeps the conn alive across long-RTT spikes, trading a brief audio glitch
// for not killing the whole session.
func (cn *Conn) writeAudio(data []byte) error {
	for {
		select {
		case cn.writeCh <- outgoing{mt: websocket.MessageBinary, data: data}:
			return nil
		case <-cn.doneCh:
			return errors.New("conn closed")
		default:
		}
		// Queue full → drop the oldest queued frame and retry. We pick the
		// oldest *audio* frame; if the head happens to be a control frame
		// we leave it (rare: text writes are < 1/s, audio is ~100/s).
		select {
		case msg := <-cn.writeCh:
			if msg.mt != websocket.MessageBinary {
				// Re-queue control message (no choice: would be unfair to
				// drop it). If channel is *still* full we'll loop and try
				// again; in practice the very next iteration will fit
				// because we already removed something.
				select {
				case cn.writeCh <- msg:
				default:
					// Channel full again, give up rather than spin: the
					// receiver is so slow that this connection is dead.
					cn.close()
					return errors.New("audio write queue jammed")
				}
			}
		case <-cn.doneCh:
			return errors.New("conn closed")
		default:
			// Race: channel was full a microsecond ago, now empty. Try again.
		}
	}
}

func (cn *Conn) writeLoop() {
	defer cn.wg.Done()
	for {
		select {
		case msg, ok := <-cn.writeCh:
			if !ok {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := cn.c.Write(ctx, msg.mt, msg.data)
			cancel()
			if err != nil {
				cn.log.Debug("ws write failed", "err", err)
				cn.close()
				return
			}
		case <-cn.doneCh:
			return
		}
	}
}

func (cn *Conn) close() {
	cn.closeOnce.Do(func() {
		close(cn.doneCh)
		_ = cn.c.Close(websocket.StatusNormalClosure, "")
	})
}

// Close blocks until the write loop exits. Used in HandleHTTP defer.
func (cn *Conn) Close() {
	cn.close()
	cn.wg.Wait()
}

// flushWriteQueue blocks (up to writeTimeout) until the write queue is empty
// and the writeLoop has had a chance to flush. Used after sending fatal-error
// frames so the client actually receives the error before the conn closes.
//
// Why this exists: writes are async (writeLoop goroutine drains the queue).
// If dispatch.run() returns immediately after sendError, the defer chain
// closes the conn before writeLoop drains — client sees a "peer close frame"
// without the error envelope and can't tell what went wrong.
func (cn *Conn) flushWriteQueue() {
	deadline := time.Now().Add(writeTimeout)
	for time.Now().Before(deadline) {
		if len(cn.writeCh) == 0 {
			// Queue empty — give writeLoop a sliver to actually emit the
			// last frame on the wire. 50 ms is plenty for a single small
			// text frame even on long-RTT links.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
