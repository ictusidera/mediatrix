package control

import (
	"net"
	"sync"
)

// boundedListener takes a slot before accepting, which caps accepted sockets
// even while clients have not sent HTTP headers or are holding keep-alives.
// The OS owns the bounded listen backlog; no goroutine is added per waiter.
type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newBoundedListener(listener net.Listener, limit int) *boundedListener {
	return &boundedListener{Listener: listener, slots: make(chan struct{}, limit), done: make(chan struct{})}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &boundedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *boundedListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.done)
		err = l.Listener.Close()
	})
	return err
}

type boundedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *boundedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.release()
	})
	return err
}
