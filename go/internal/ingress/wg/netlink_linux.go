// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"
)

// recvTimeout bounds the wait for each answer of the kernel.
const recvTimeout = 10 * time.Second

// recvBufLen is the buffer of a read: the kernel fills a dump's reads up
// to 32 KiB.
const recvBufLen = 64 * 1024

// dumpRetries is how often a dump that the kernel marks interrupted, by a
// change while it ran, is tried again.
const dumpRetries = 5

var errDumpInterrupted = errors.New("netlink dump interrupted by a change")

// kernelError is the kernel's refusal of a request. It unwraps to the
// errno, so errors.Is(err, syscall.EEXIST) works.
type kernelError struct {
	errno syscall.Errno
	// text is the message of the extended acknowledgement, if any.
	text string
}

func (e *kernelError) Error() string {
	if e.text != "" {
		return e.errno.Error() + ": " + e.text
	}
	return e.errno.Error()
}

func (e *kernelError) Unwrap() error { return e.errno }

// conn is a netlink socket, opened in the network namespace of the
// calling thread, for one request after the other.
type conn struct {
	fd  int
	pid uint32
	seq uint32
}

// dial opens a netlink socket of protocol proto (netlinkRoute or
// netlinkGeneric).
func dial(proto int) (*conn, error) {
	fd, err := syscall.Socket(afNetlink, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("open a netlink socket: %w", err)
	}
	c := &conn{fd: fd}
	// The kernel's messages with refusals, and acknowledgements without
	// the request echoed; older kernels lack them, which only costs
	// detail.
	_ = syscall.SetsockoptInt(fd, solNetlink, netlinkExtAck, 1)
	_ = syscall.SetsockoptInt(fd, solNetlink, netlinkCapAck, 1)
	tv := syscall.NsecToTimeval(recvTimeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		_ = c.close()
		return nil, fmt.Errorf("set the netlink socket's timeout: %w", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: afNetlink}); err != nil {
		_ = c.close()
		return nil, fmt.Errorf("bind a netlink socket: %w", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = c.close()
		return nil, fmt.Errorf("netlink socket's address: %w", err)
	}
	nl, ok := sa.(*syscall.SockaddrNetlink)
	if !ok {
		_ = c.close()
		return nil, fmt.Errorf("netlink socket's address is a %T", sa)
	}
	c.pid = nl.Pid
	return c, nil
}

func (c *conn) close() error { return syscall.Close(c.fd) }

// execute sends the request msg and returns the kernel's answers other
// than the acknowledgement and the end of a dump. A request that is not a
// dump must ask for an acknowledgement (nlmFAck): that ends the wait. A
// dump that a change interrupted is tried again.
func (c *conn) execute(msg []byte) ([]message, error) {
	for attempt := 1; ; attempt++ {
		msgs, err := c.try(msg)
		if !errors.Is(err, errDumpInterrupted) || attempt == dumpRetries {
			return msgs, err
		}
	}
}

func (c *conn) try(msg []byte) ([]message, error) {
	c.seq++
	seq := c.seq
	ne.PutUint32(msg[8:], seq)
	dump := ne.Uint16(msg[6:])&nlmFDump == nlmFDump
	if err := syscall.Sendto(c.fd, msg, 0, &syscall.SockaddrNetlink{Family: afNetlink}); err != nil {
		return nil, fmt.Errorf("send to netlink: %w", err)
	}
	var out []message
	interrupted := false
	buf := make([]byte, recvBufLen)
	for {
		n, _, flags, from, err := syscall.Recvmsg(c.fd, buf, nil, 0)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EAGAIN):
			return nil, fmt.Errorf("netlink: no answer from the kernel in %s", recvTimeout)
		case err != nil:
			return nil, fmt.Errorf("receive from netlink: %w", err)
		case flags&syscall.MSG_TRUNC != 0:
			return nil, fmt.Errorf("netlink answer larger than %d bytes", len(buf))
		}
		if nl, ok := from.(*syscall.SockaddrNetlink); !ok || nl.Pid != 0 {
			continue // not from the kernel
		}
		msgs, err := parseMessages(buf[:n])
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.seq != seq || m.pid != c.pid {
				continue // an answer to an earlier request
			}
			if m.flags&nlmFDumpIntr != 0 {
				interrupted = true
			}
			switch m.typ {
			case nlmsgNoop:
			case nlmsgError:
				code, text, err := parseError(m)
				if err != nil {
					return nil, err
				}
				if code != 0 {
					return nil, &kernelError{errno: syscall.Errno(-code), text: text}
				}
				if !dump {
					return out, nil
				}
			case nlmsgDone:
				if !dump {
					continue
				}
				// Since Linux 4.x the end of a dump carries its error.
				if len(m.data) >= 4 {
					if code := int32(ne.Uint32(m.data)); code < 0 {
						return nil, &kernelError{errno: syscall.Errno(-code)}
					}
				}
				if interrupted {
					return nil, errDumpInterrupted
				}
				return out, nil
			default:
				// The next read reuses buf.
				m.data = slices.Clone(m.data)
				out = append(out, m)
			}
		}
	}
}

// request opens a socket of protocol proto, sends msg and closes it.
func request(proto int, msg []byte) ([]message, error) {
	c, err := dial(proto)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.close() }()
	return c.execute(msg)
}
