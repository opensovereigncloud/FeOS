// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"io"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	portforwardv1 "github.com/ironcore-dev/feos/go/gen/feos/portforward/v1"
)

// forwardChunk is the largest data frame toward the caller.
const forwardChunk = 16 << 10

// forwardService is the guest's PortForwardService: a forward of a port
// dials a local address (127.0.0.1:<port>, or what the script maps the
// port to) and pipes bytes both ways.
type forwardService struct {
	portforwardv1.UnimplementedPortForwardServiceServer
	n *Node
}

func (f *forwardService) Forward(stream grpc.BidiStreamingServer[portforwardv1.ForwardRequest, portforwardv1.ForwardResponse]) error {
	ctx := stream.Context()
	g, err := guestFrom(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "stream ended before `open`")
	}
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first frame must be `open`")
	}
	port := open.Port
	switch {
	case port > 65535:
		return status.Errorf(codes.InvalidArgument, "port %d out of range", port)
	case port == 0:
		return status.Error(codes.InvalidArgument, "port 0 is not a port")
	case f.n.script.isKubePort(g.uid, port):
		return status.Errorf(codes.FailedPrecondition, "port %d is a kube listener", port)
	}
	// Dial before answering: "nothing listens there" reaches the caller as
	// a failed call, in the guest's words.
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", f.n.script.portAddr(g.uid, port))
	if err != nil {
		return status.Errorf(codes.Unavailable, "127.0.0.1:%d: %v", port, err)
	}
	defer conn.Close()
	// A cancelled call drops the connection, also after a half-close.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := stream.Send(&portforwardv1.ForwardResponse{Frame: &portforwardv1.ForwardResponse_Ready{Ready: &portforwardv1.Ready{}}}); err != nil {
		return err
	}

	// caller → guest: the end of the request stream is a half-close;
	// anything else drops the connection.
	go func() {
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
				return
			}
			if err != nil {
				conn.Close()
				return
			}
			if data := req.GetData(); len(data) > 0 {
				if _, err := conn.Write(data); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	// guest → caller, until the guest closes its side.
	buf := make([]byte, forwardChunk)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if serr := stream.Send(&portforwardv1.ForwardResponse{Frame: &portforwardv1.ForwardResponse_Data{Data: data}}); serr != nil {
				return serr
			}
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return status.Errorf(codes.Internal, "read from guest: %v", err)
		}
	}
}
