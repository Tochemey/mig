// MIT License
//
// Copyright (c) 2026 Arsene Tochemey Gandote
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//

package harness

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// pollInterval bounds how long a forwarding loop can sit in a read before it
// notices the link has been cut.
const pollInterval = 20 * time.Millisecond

// Proxy forwards TCP to Postgres and can stop delivering, on demand.
//
// A partition is not a closed connection. A killed process, a terminated
// backend and a dropped route all look different to the runner: the first two
// deliver an error at once, while a partition delivers nothing at all and the
// caller waits on a socket that will never answer. Only the last one shows
// whether a lease holder gives up its lease before it expires, so the runner
// has to meet a link that goes quiet rather than one that closes.
type Proxy struct {
	listener  net.Listener
	target    string
	cut       atomic.Bool
	wg        sync.WaitGroup
	closeOnce sync.Once

	mu    sync.Mutex
	conns []net.Conn
}

// NewProxy listens on an arbitrary local port and forwards to target, which is
// a host:port.
func NewProxy(target string) (*Proxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	p := &Proxy{listener: listener, target: target}

	p.wg.Add(1)

	go func() {
		defer p.wg.Done()

		p.accept()
	}()

	return p, nil
}

// Addr is the host:port callers connect to.
func (p *Proxy) Addr() string {
	return p.listener.Addr().String()
}

// DSN rewrites a connection string to route through the proxy.
func (p *Proxy) DSN(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}

	u.Host = p.Addr()

	return u.String(), nil
}

// Cut stops delivering in both directions, leaving every socket open.
//
// Neither end is told. The client's writes land in a buffer nobody drains and
// its reads never complete, which is what a lost route looks like from inside
// a process that is otherwise healthy.
func (p *Proxy) Cut() {
	p.cut.Store(true)
}

// Close stops the proxy and drops every connection through it.
func (p *Proxy) Close() error {
	var err error

	p.closeOnce.Do(func() {
		err = p.listener.Close()

		p.mu.Lock()

		for _, conn := range p.conns {
			err = errors.Join(err, ignoreClosed(conn.Close()))
		}

		p.conns = nil

		p.mu.Unlock()

		p.wg.Wait()
	})

	if err != nil {
		return fmt.Errorf("close proxy: %w", err)
	}

	return nil
}

// accept serves connections until the listener closes.
func (p *Proxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}

		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()

			continue
		}

		p.track(client, server)

		p.wg.Add(2)

		go func() {
			defer p.wg.Done()

			p.forward(client, server)
		}()

		go func() {
			defer p.wg.Done()

			p.forward(server, client)
		}()
	}
}

// track records a pair so Close can drop them.
func (p *Proxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.conns = append(p.conns, conns...)
}

// forward copies from src to dst until the link is cut or either side goes
// away.
//
// Reads carry a deadline so that a quiet connection still lets the loop notice
// the cut. Once cut, the loop returns without closing anything: the sockets
// stay open and stop carrying traffic, which is the point.
func (p *Proxy) forward(src, dst net.Conn) {
	buf := make([]byte, 32*1024)

	for {
		if p.cut.Load() {
			return
		}

		if err := src.SetReadDeadline(time.Now().Add(pollInterval)); err != nil {
			return
		}

		n, readErr := src.Read(buf)

		if n > 0 {
			if p.cut.Load() {
				return
			}

			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}

		if readErr == nil {
			continue
		}

		var netErr net.Error
		if errors.As(readErr, &netErr) && netErr.Timeout() {
			continue
		}

		return
	}
}

// ignoreClosed drops the error a second close reports.
func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
