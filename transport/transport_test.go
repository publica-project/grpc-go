/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/publica-project/grpc/codes"
	"github.com/publica-project/grpc/keepalive"
	"github.com/publica-project/grpc/status"
	"golang.org/x/net/context"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type server struct {
	lis        net.Listener
	port       string
	startedErr chan error // error (or nil) with server start value
	mu         sync.Mutex
	conns      map[ServerTransport]bool
	h          *testStreamHandler
}

var (
	expectedRequest            = []byte("ping")
	expectedResponse           = []byte("pong")
	expectedRequestLarge       = make([]byte, initialWindowSize*2)
	expectedResponseLarge      = make([]byte, initialWindowSize*2)
	expectedInvalidHeaderField = "invalid/content-type"
)

type testStreamHandler struct {
	t      *http2Server
	notify chan struct{}
}

type hType int

const (
	normal hType = iota
	suspended
	notifyCall
	misbehaved
	encodingRequiredStatus
	invalidHeaderField
	delayRead
	delayWrite
	pingpong
)

func (h *testStreamHandler) handleStreamAndNotify(s *Stream) {
	if h.notify == nil {
		return
	}
	go func() {
		select {
		case <-h.notify:
		default:
			close(h.notify)
		}
	}()
}

func (h *testStreamHandler) handleStream(t *testing.T, s *Stream) {
	req := expectedRequest
	resp := expectedResponse
	if s.Method() == "foo.Large" {
		req = expectedRequestLarge
		resp = expectedResponseLarge
	}
	p := make([]byte, len(req))
	_, err := s.Read(p)
	if err != nil {
		return
	}
	if !bytes.Equal(p, req) {
		t.Fatalf("handleStream got %v, want %v", p, req)
	}
	// send a response back to the client.
	h.t.Write(s, nil, resp, &Options{})
	// send the trailer to end the stream.
	h.t.WriteStatus(s, status.New(codes.OK, ""))
}

func (h *testStreamHandler) handleStreamPingPong(t *testing.T, s *Stream) {
	header := make([]byte, 5)
	for {
		if _, err := s.Read(header); err != nil {
			if err == io.EOF {
				h.t.WriteStatus(s, status.New(codes.OK, ""))
				return
			}
			t.Fatalf("Error on server while reading data header: %v", err)
		}
		sz := binary.BigEndian.Uint32(header[1:])
		msg := make([]byte, int(sz))
		if _, err := s.Read(msg); err != nil {
			t.Fatalf("Error on server while reading message: %v", err)
		}
		buf := make([]byte, sz+5)
		buf[0] = byte(0)
		binary.BigEndian.PutUint32(buf[1:], uint32(sz))
		copy(buf[5:], msg)
		h.t.Write(s, nil, buf, &Options{})
	}
}

func (h *testStreamHandler) handleStreamMisbehave(t *testing.T, s *Stream) {
	conn, ok := s.ServerTransport().(*http2Server)
	if !ok {
		t.Fatalf("Failed to convert %v to *http2Server", s.ServerTransport())
	}
	var sent int
	p := make([]byte, http2MaxFrameLen)
	for sent < initialWindowSize {
		n := initialWindowSize - sent
		// The last message may be smaller than http2MaxFrameLen
		if n <= http2MaxFrameLen {
			if s.Method() == "foo.Connection" {
				// Violate connection level flow control window of client but do not
				// violate any stream level windows.
				p = make([]byte, n)
			} else {
				// Violate stream level flow control window of client.
				p = make([]byte, n+1)
			}
		}
		conn.controlBuf.put(&dataFrame{s.id, false, p, func() {}})
		sent += len(p)
	}
}

func (h *testStreamHandler) handleStreamEncodingRequiredStatus(t *testing.T, s *Stream) {
	// raw newline is not accepted by http2 framer so it must be encoded.
	h.t.WriteStatus(s, encodingTestStatus)
}

func (h *testStreamHandler) handleStreamInvalidHeaderField(t *testing.T, s *Stream) {
	headerFields := []hpack.HeaderField{}
	headerFields = append(headerFields, hpack.HeaderField{Name: "content-type", Value: expectedInvalidHeaderField})
	h.t.controlBuf.put(&headerFrame{
		streamID:  s.id,
		hf:        headerFields,
		endStream: false,
	})
}

func (h *testStreamHandler) handleStreamDelayRead(t *testing.T, s *Stream) {
	req := expectedRequest
	resp := expectedResponse
	if s.Method() == "foo.Large" {
		req = expectedRequestLarge
		resp = expectedResponseLarge
	}
	p := make([]byte, len(req))

	// Wait before reading. Give time to client to start sending
	// before server starts reading.
	time.Sleep(2 * time.Second)
	_, err := s.Read(p)
	if err != nil {
		t.Fatalf("s.Read(_) = _, %v, want _, <nil>", err)
		return
	}

	if !bytes.Equal(p, req) {
		t.Fatalf("handleStream got %v, want %v", p, req)
	}
	// send a response back to the client.
	h.t.Write(s, nil, resp, &Options{})
	// send the trailer to end the stream.
	h.t.WriteStatus(s, status.New(codes.OK, ""))
}

func (h *testStreamHandler) handleStreamDelayWrite(t *testing.T, s *Stream) {
	req := expectedRequest
	resp := expectedResponse
	if s.Method() == "foo.Large" {
		req = expectedRequestLarge
		resp = expectedResponseLarge
	}
	p := make([]byte, len(req))
	_, err := s.Read(p)
	if err != nil {
		t.Fatalf("s.Read(_) = _, %v, want _, <nil>", err)
		return
	}
	if !bytes.Equal(p, req) {
		t.Fatalf("handleStream got %v, want %v", p, req)
	}

	// Wait before sending. Give time to client to start reading
	// before server starts sending.
	time.Sleep(2 * time.Second)
	h.t.Write(s, nil, resp, &Options{})
	// send the trailer to end the stream.
	h.t.WriteStatus(s, status.New(codes.OK, ""))
}

// start starts server. Other goroutines should block on s.readyChan for further operations.
func (s *server) start(t *testing.T, port int, serverConfig *ServerConfig, ht hType) {
	var err error
	if port == 0 {
		s.lis, err = net.Listen("tcp", "localhost:0")
	} else {
		s.lis, err = net.Listen("tcp", "localhost:"+strconv.Itoa(port))
	}
	if err != nil {
		s.startedErr <- fmt.Errorf("failed to listen: %v", err)
		return
	}
	_, p, err := net.SplitHostPort(s.lis.Addr().String())
	if err != nil {
		s.startedErr <- fmt.Errorf("failed to parse listener address: %v", err)
		return
	}
	s.port = p
	s.conns = make(map[ServerTransport]bool)
	s.startedErr <- nil
	for {
		conn, err := s.lis.Accept()
		if err != nil {
			return
		}
		transport, err := NewServerTransport("http2", conn, serverConfig)
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.conns == nil {
			s.mu.Unlock()
			transport.Close()
			return
		}
		s.conns[transport] = true
		h := &testStreamHandler{t: transport.(*http2Server)}
		s.h = h
		s.mu.Unlock()
		switch ht {
		case notifyCall:
			go transport.HandleStreams(h.handleStreamAndNotify,
				func(ctx context.Context, _ string) context.Context {
					return ctx
				})
		case suspended:
			go transport.HandleStreams(func(*Stream) {}, // Do nothing to handle the stream.
				func(ctx context.Context, method string) context.Context {
					return ctx
				})
		case misbehaved:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamMisbehave(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		case encodingRequiredStatus:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamEncodingRequiredStatus(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		case invalidHeaderField:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamInvalidHeaderField(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		case delayRead:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamDelayRead(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		case delayWrite:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamDelayWrite(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		case pingpong:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStreamPingPong(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		default:
			go transport.HandleStreams(func(s *Stream) {
				go h.handleStream(t, s)
			}, func(ctx context.Context, method string) context.Context {
				return ctx
			})
		}
	}
}

func (s *server) wait(t *testing.T, timeout time.Duration) {
	select {
	case err := <-s.startedErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(timeout):
		t.Fatalf("Timed out after %v waiting for server to be ready", timeout)
	}
}

func (s *server) stop() {
	s.lis.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.conns = nil
	s.mu.Unlock()
}

func setUp(t *testing.T, port int, maxStreams uint32, ht hType) (*server, ClientTransport) {
	return setUpWithOptions(t, port, &ServerConfig{MaxStreams: maxStreams}, ht, ConnectOptions{})
}

func setUpWithOptions(t *testing.T, port int, serverConfig *ServerConfig, ht hType, copts ConnectOptions) (*server, ClientTransport) {
	server := &server{startedErr: make(chan error, 1)}
	go server.start(t, port, serverConfig, ht)
	server.wait(t, 2*time.Second)
	addr := "localhost:" + server.port
	var (
		ct      ClientTransport
		connErr error
	)
	target := TargetInfo{
		Addr: addr,
	}
	connectCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	ct, connErr = NewClientTransport(connectCtx, context.Background(), target, copts, func() {})
	if connErr != nil {
		cancel() // Do not cancel in success path.
		t.Fatalf("failed to create transport: %v", connErr)
	}
	return server, ct
}

func setUpWithNoPingServer(t *testing.T, copts ConnectOptions, done chan net.Conn) ClientTransport {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}
	// Launch a non responsive server.
	go func() {
		defer lis.Close()
		conn, err := lis.Accept()
		if err != nil {
			t.Errorf("Error at server-side while accepting: %v", err)
			close(done)
			return
		}
		done <- conn
	}()
	connectCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	tr, err := NewClientTransport(connectCtx, context.Background(), TargetInfo{Addr: lis.Addr().String()}, copts, func() {})
	if err != nil {
		cancel() // Do not cancel in success path.
		// Server clean-up.
		lis.Close()
		if conn, ok := <-done; ok {
			conn.Close()
		}
		t.Fatalf("Failed to dial: %v", err)
	}
	return tr
}

// TestInflightStreamClosing ensures that closing in-flight stream
// sends StreamError to concurrent stream reader.
func TestInflightStreamClosing(t *testing.T) {
	serverConfig := &ServerConfig{}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()

	stream, err := client.NewStream(context.Background(), &CallHdr{})
	if err != nil {
		t.Fatalf("Client failed to create RPC request: %v", err)
	}

	donec := make(chan struct{})
	serr := StreamError{Desc: "client connection is closing"}
	go func() {
		defer close(donec)
		if _, err := stream.Read(make([]byte, defaultWindowSize)); err != serr {
			t.Errorf("unexpected Stream error %v, expected %v", err, serr)
		}
	}()

	// should unblock concurrent stream.Read
	client.CloseStream(stream, serr)

	// wait for stream.Read error
	timeout := time.NewTimer(5 * time.Second)
	select {
	case <-donec:
		if !timeout.Stop() {
			<-timeout.C
		}
	case <-timeout.C:
		t.Fatalf("Test timed out, expected a StreamError.")
	}
}

// TestMaxConnectionIdle tests that a server will send GoAway to a idle client.
// An idle client is one who doesn't make any RPC calls for a duration of
// MaxConnectionIdle time.
func TestMaxConnectionIdle(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepaliveParams: keepalive.ServerParameters{
			MaxConnectionIdle: 2 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()
	stream, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Client failed to create RPC request: %v", err)
	}
	stream.mu.Lock()
	stream.rstStream = true
	stream.mu.Unlock()
	client.CloseStream(stream, nil)
	// wait for server to see that closed stream and max-age logic to send goaway after no new RPCs are mode
	timeout := time.NewTimer(time.Second * 4)
	select {
	case <-client.GoAway():
		if !timeout.Stop() {
			<-timeout.C
		}
	case <-timeout.C:
		t.Fatalf("Test timed out, expected a GoAway from the server.")
	}
}

// TestMaxConenctionIdleNegative tests that a server will not send GoAway to a non-idle(busy) client.
func TestMaxConnectionIdleNegative(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepaliveParams: keepalive.ServerParameters{
			MaxConnectionIdle: 2 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()
	_, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Client failed to create RPC request: %v", err)
	}
	timeout := time.NewTimer(time.Second * 4)
	select {
	case <-client.GoAway():
		if !timeout.Stop() {
			<-timeout.C
		}
		t.Fatalf("A non-idle client received a GoAway.")
	case <-timeout.C:
	}

}

// TestMaxConnectionAge tests that a server will send GoAway after a duration of MaxConnectionAge.
func TestMaxConnectionAge(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepaliveParams: keepalive.ServerParameters{
			MaxConnectionAge: 2 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()
	_, err := client.NewStream(context.Background(), &CallHdr{})
	if err != nil {
		t.Fatalf("Client failed to create stream: %v", err)
	}
	// Wait for max-age logic to send GoAway.
	timeout := time.NewTimer(4 * time.Second)
	select {
	case <-client.GoAway():
		if !timeout.Stop() {
			<-timeout.C
		}
	case <-timeout.C:
		t.Fatalf("Test timer out, expected a GoAway from the server.")
	}
}

// TestKeepaliveServer tests that a server closes connection with a client that doesn't respond to keepalive pings.
func TestKeepaliveServer(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepaliveParams: keepalive.ServerParameters{
			Time:    2 * time.Second,
			Timeout: 1 * time.Second,
		},
	}
	server, c := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer c.Close()
	client, err := net.Dial("tcp", server.lis.Addr().String())
	if err != nil {
		t.Fatalf("Failed to dial: %v", err)
	}
	defer client.Close()

	// Set read deadline on client conn so that it doesn't block forever in errorsome cases.
	client.SetDeadline(time.Now().Add(10 * time.Second))

	if n, err := client.Write(clientPreface); err != nil || n != len(clientPreface) {
		t.Fatalf("Error writing client preface; n=%v, err=%v", n, err)
	}
	framer := newFramer(client, defaultWriteBufSize, defaultReadBufSize)
	if err := framer.fr.WriteSettings(http2.Setting{}); err != nil {
		t.Fatal("Error writing settings frame:", err)
	}
	framer.writer.Flush()
	// Wait for keepalive logic to close the connection.
	time.Sleep(4 * time.Second)
	b := make([]byte, 24)
	for {
		_, err = client.Read(b)
		if err == nil {
			continue
		}
		if err != io.EOF {
			t.Fatalf("client.Read(_) = _,%v, want io.EOF", err)
		}
		break
	}
}

// TestKeepaliveServerNegative tests that a server doesn't close connection with a client that responds to keepalive pings.
func TestKeepaliveServerNegative(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepaliveParams: keepalive.ServerParameters{
			Time:    2 * time.Second,
			Timeout: 1 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()
	// Give keepalive logic some time by sleeping.
	time.Sleep(4 * time.Second)
	// Assert that client is still active.
	clientTr := client.(*http2Client)
	clientTr.mu.Lock()
	defer clientTr.mu.Unlock()
	if clientTr.state != reachable {
		t.Fatalf("Test failed: Expected server-client connection to be healthy.")
	}
}

func TestKeepaliveClientClosesIdleTransport(t *testing.T) {
	done := make(chan net.Conn, 1)
	tr := setUpWithNoPingServer(t, ConnectOptions{KeepaliveParams: keepalive.ClientParameters{
		Time:                2 * time.Second, // Keepalive time = 2 sec.
		Timeout:             1 * time.Second, // Keepalive timeout = 1 sec.
		PermitWithoutStream: true,            // Run keepalive even with no RPCs.
	}}, done)
	defer tr.Close()
	conn, ok := <-done
	if !ok {
		t.Fatalf("Server didn't return connection object")
	}
	defer conn.Close()
	// Sleep for keepalive to close the connection.
	time.Sleep(4 * time.Second)
	// Assert that the connection was closed.
	ct := tr.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state == reachable {
		t.Fatalf("Test Failed: Expected client transport to have closed.")
	}
}

func TestKeepaliveClientStaysHealthyOnIdleTransport(t *testing.T) {
	done := make(chan net.Conn, 1)
	tr := setUpWithNoPingServer(t, ConnectOptions{KeepaliveParams: keepalive.ClientParameters{
		Time:    2 * time.Second, // Keepalive time = 2 sec.
		Timeout: 1 * time.Second, // Keepalive timeout = 1 sec.
	}}, done)
	defer tr.Close()
	conn, ok := <-done
	if !ok {
		t.Fatalf("server didn't reutrn connection object")
	}
	defer conn.Close()
	// Give keepalive some time.
	time.Sleep(4 * time.Second)
	// Assert that connections is still healthy.
	ct := tr.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state != reachable {
		t.Fatalf("Test failed: Expected client transport to be healthy.")
	}
}

func TestKeepaliveClientClosesWithActiveStreams(t *testing.T) {
	done := make(chan net.Conn, 1)
	tr := setUpWithNoPingServer(t, ConnectOptions{KeepaliveParams: keepalive.ClientParameters{
		Time:    2 * time.Second, // Keepalive time = 2 sec.
		Timeout: 1 * time.Second, // Keepalive timeout = 1 sec.
	}}, done)
	defer tr.Close()
	conn, ok := <-done
	if !ok {
		t.Fatalf("Server didn't return connection object")
	}
	defer conn.Close()
	// Create a stream.
	_, err := tr.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Failed to create a new stream: %v", err)
	}
	// Give keepalive some time.
	time.Sleep(4 * time.Second)
	// Assert that transport was closed.
	ct := tr.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state == reachable {
		t.Fatalf("Test failed: Expected client transport to have closed.")
	}
}

func TestKeepaliveClientStaysHealthyWithResponsiveServer(t *testing.T) {
	s, tr := setUpWithOptions(t, 0, &ServerConfig{MaxStreams: math.MaxUint32}, normal, ConnectOptions{KeepaliveParams: keepalive.ClientParameters{
		Time:                2 * time.Second, // Keepalive time = 2 sec.
		Timeout:             1 * time.Second, // Keepalive timeout = 1 sec.
		PermitWithoutStream: true,            // Run keepalive even with no RPCs.
	}})
	defer s.stop()
	defer tr.Close()
	// Give keep alive some time.
	time.Sleep(4 * time.Second)
	// Assert that transport is healthy.
	ct := tr.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state != reachable {
		t.Fatalf("Test failed: Expected client transport to be healthy.")
	}
}

func TestKeepaliveServerEnforcementWithAbusiveClientNoRPC(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepalivePolicy: keepalive.EnforcementPolicy{
			MinTime: 2 * time.Second,
		},
	}
	clientOptions := ConnectOptions{
		KeepaliveParams: keepalive.ClientParameters{
			Time:                50 * time.Millisecond,
			Timeout:             1 * time.Second,
			PermitWithoutStream: true,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, normal, clientOptions)
	defer server.stop()
	defer client.Close()

	timeout := time.NewTimer(10 * time.Second)
	select {
	case <-client.GoAway():
		if !timeout.Stop() {
			<-timeout.C
		}
	case <-timeout.C:
		t.Fatalf("Test failed: Expected a GoAway from server.")
	}
	time.Sleep(500 * time.Millisecond)
	ct := client.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state == reachable {
		t.Fatalf("Test failed: Expected the connection to be closed.")
	}
}

func TestKeepaliveServerEnforcementWithAbusiveClientWithRPC(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepalivePolicy: keepalive.EnforcementPolicy{
			MinTime: 2 * time.Second,
		},
	}
	clientOptions := ConnectOptions{
		KeepaliveParams: keepalive.ClientParameters{
			Time:    50 * time.Millisecond,
			Timeout: 1 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, clientOptions)
	defer server.stop()
	defer client.Close()

	if _, err := client.NewStream(context.Background(), &CallHdr{Flush: true}); err != nil {
		t.Fatalf("Client failed to create stream.")
	}
	timeout := time.NewTimer(10 * time.Second)
	select {
	case <-client.GoAway():
		if !timeout.Stop() {
			<-timeout.C
		}
	case <-timeout.C:
		t.Fatalf("Test failed: Expected a GoAway from server.")
	}
	time.Sleep(500 * time.Millisecond)
	ct := client.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state == reachable {
		t.Fatalf("Test failed: Expected the connection to be closed.")
	}
}

func TestKeepaliveServerEnforcementWithObeyingClientNoRPC(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepalivePolicy: keepalive.EnforcementPolicy{
			MinTime:             100 * time.Millisecond,
			PermitWithoutStream: true,
		},
	}
	clientOptions := ConnectOptions{
		KeepaliveParams: keepalive.ClientParameters{
			Time:                101 * time.Millisecond,
			Timeout:             1 * time.Second,
			PermitWithoutStream: true,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, normal, clientOptions)
	defer server.stop()
	defer client.Close()

	// Give keepalive enough time.
	time.Sleep(3 * time.Second)
	// Assert that connection is healthy.
	ct := client.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state != reachable {
		t.Fatalf("Test failed: Expected connection to be healthy.")
	}
}

func TestKeepaliveServerEnforcementWithObeyingClientWithRPC(t *testing.T) {
	serverConfig := &ServerConfig{
		KeepalivePolicy: keepalive.EnforcementPolicy{
			MinTime: 100 * time.Millisecond,
		},
	}
	clientOptions := ConnectOptions{
		KeepaliveParams: keepalive.ClientParameters{
			Time:    101 * time.Millisecond,
			Timeout: 1 * time.Second,
		},
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, clientOptions)
	defer server.stop()
	defer client.Close()

	if _, err := client.NewStream(context.Background(), &CallHdr{Flush: true}); err != nil {
		t.Fatalf("Client failed to create stream.")
	}

	// Give keepalive enough time.
	time.Sleep(3 * time.Second)
	// Assert that connection is healthy.
	ct := client.(*http2Client)
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.state != reachable {
		t.Fatalf("Test failed: Expected connection to be healthy.")
	}
}

func TestClientSendAndReceive(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, normal)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Small",
	}
	s1, err1 := ct.NewStream(context.Background(), callHdr)
	if err1 != nil {
		t.Fatalf("failed to open stream: %v", err1)
	}
	if s1.id != 1 {
		t.Fatalf("wrong stream id: %d", s1.id)
	}
	s2, err2 := ct.NewStream(context.Background(), callHdr)
	if err2 != nil {
		t.Fatalf("failed to open stream: %v", err2)
	}
	if s2.id != 3 {
		t.Fatalf("wrong stream id: %d", s2.id)
	}
	opts := Options{
		Last:  true,
		Delay: false,
	}
	if err := ct.Write(s1, nil, expectedRequest, &opts); err != nil && err != io.EOF {
		t.Fatalf("failed to send data: %v", err)
	}
	p := make([]byte, len(expectedResponse))
	_, recvErr := s1.Read(p)
	if recvErr != nil || !bytes.Equal(p, expectedResponse) {
		t.Fatalf("Error: %v, want <nil>; Result: %v, want %v", recvErr, p, expectedResponse)
	}
	_, recvErr = s1.Read(p)
	if recvErr != io.EOF {
		t.Fatalf("Error: %v; want <EOF>", recvErr)
	}
	ct.Close()
	server.stop()
}

func TestClientErrorNotify(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, normal)
	go server.stop()
	// ct.reader should detect the error and activate ct.Error().
	<-ct.Error()
	ct.Close()
}

func performOneRPC(ct ClientTransport) {
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Small",
	}
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		return
	}
	opts := Options{
		Last:  true,
		Delay: false,
	}
	if err := ct.Write(s, []byte{}, expectedRequest, &opts); err == nil || err == io.EOF {
		time.Sleep(5 * time.Millisecond)
		// The following s.Recv()'s could error out because the
		// underlying transport is gone.
		//
		// Read response
		p := make([]byte, len(expectedResponse))
		s.Read(p)
		// Read io.EOF
		s.Read(p)
	}
}

func TestClientMix(t *testing.T) {
	s, ct := setUp(t, 0, math.MaxUint32, normal)
	go func(s *server) {
		time.Sleep(5 * time.Second)
		s.stop()
	}(s)
	go func(ct ClientTransport) {
		<-ct.Error()
		ct.Close()
	}(ct)
	for i := 0; i < 1000; i++ {
		time.Sleep(10 * time.Millisecond)
		go performOneRPC(ct)
	}
}

func TestLargeMessage(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, normal)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Large",
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ct.NewStream(context.Background(), callHdr)
			if err != nil {
				t.Errorf("%v.NewStream(_, _) = _, %v, want _, <nil>", ct, err)
			}
			if err := ct.Write(s, []byte{}, expectedRequestLarge, &Options{Last: true, Delay: false}); err != nil && err != io.EOF {
				t.Errorf("%v.Write(_, _, _) = %v, want  <nil>", ct, err)
			}
			p := make([]byte, len(expectedResponseLarge))
			if _, err := s.Read(p); err != nil || !bytes.Equal(p, expectedResponseLarge) {
				t.Errorf("s.Read(%v) = _, %v, want %v, <nil>", err, p, expectedResponse)
			}
			if _, err = s.Read(p); err != io.EOF {
				t.Errorf("Failed to complete the stream %v; want <EOF>", err)
			}
		}()
	}
	wg.Wait()
	ct.Close()
	server.stop()
}

func TestLargeMessageWithDelayRead(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, delayRead)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Large",
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ct.NewStream(context.Background(), callHdr)
			if err != nil {
				t.Errorf("%v.NewStream(_, _) = _, %v, want _, <nil>", ct, err)
			}
			if err := ct.Write(s, []byte{}, expectedRequestLarge, &Options{Last: true, Delay: false}); err != nil && err != io.EOF {
				t.Errorf("%v.Write(_, _, _) = %v, want  <nil>", ct, err)
			}
			p := make([]byte, len(expectedResponseLarge))

			// Give time to server to begin sending before client starts reading.
			time.Sleep(2 * time.Second)
			if _, err := s.Read(p); err != nil || !bytes.Equal(p, expectedResponseLarge) {
				t.Errorf("s.Read(_) = _, %v, want _, <nil>", err)
			}
			if _, err = s.Read(p); err != io.EOF {
				t.Errorf("Failed to complete the stream %v; want <EOF>", err)
			}
		}()
	}
	wg.Wait()
	ct.Close()
	server.stop()
}

func TestLargeMessageDelayWrite(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, delayWrite)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Large",
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ct.NewStream(context.Background(), callHdr)
			if err != nil {
				t.Errorf("%v.NewStream(_, _) = _, %v, want _, <nil>", ct, err)
			}

			// Give time to server to start reading before client starts sending.
			time.Sleep(2 * time.Second)
			if err := ct.Write(s, []byte{}, expectedRequestLarge, &Options{Last: true, Delay: false}); err != nil && err != io.EOF {
				t.Errorf("%v.Write(_, _, _) = %v, want  <nil>", ct, err)
			}
			p := make([]byte, len(expectedResponseLarge))
			if _, err := s.Read(p); err != nil || !bytes.Equal(p, expectedResponseLarge) {
				t.Errorf("io.ReadFull(%v) = _, %v, want %v, <nil>", err, p, expectedResponse)
			}
			if _, err = s.Read(p); err != io.EOF {
				t.Errorf("Failed to complete the stream %v; want <EOF>", err)
			}
		}()
	}
	wg.Wait()
	ct.Close()
	server.stop()
}

func TestGracefulClose(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, normal)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Small",
	}
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("%v.NewStream(_, _) = _, %v, want _, <nil>", ct, err)
	}
	if err = ct.GracefulClose(); err != nil {
		t.Fatalf("%v.GracefulClose() = %v, want <nil>", ct, err)
	}
	var wg sync.WaitGroup
	// Expect the failure for all the follow-up streams because ct has been closed gracefully.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ct.NewStream(context.Background(), callHdr); err != errStreamDrain {
				t.Errorf("%v.NewStream(_, _) = _, %v, want _, %v", ct, err, errStreamDrain)
			}
		}()
	}
	opts := Options{
		Last:  true,
		Delay: false,
	}
	// The stream which was created before graceful close can still proceed.
	if err := ct.Write(s, nil, expectedRequest, &opts); err != nil && err != io.EOF {
		t.Fatalf("%v.Write(_, _, _) = %v, want  <nil>", ct, err)
	}
	p := make([]byte, len(expectedResponse))
	if _, err := s.Read(p); err != nil || !bytes.Equal(p, expectedResponse) {
		t.Fatalf("s.Read(%v) = _, %v, want %v, <nil>", err, p, expectedResponse)
	}
	if _, err = s.Read(p); err != io.EOF {
		t.Fatalf("Failed to complete the stream %v; want <EOF>", err)
	}
	wg.Wait()
	ct.Close()
	server.stop()
}

func TestLargeMessageSuspension(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, suspended)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Large",
	}
	// Set a long enough timeout for writing a large message out.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := ct.NewStream(ctx, callHdr)
	if err != nil {
		t.Fatalf("failed to open stream: %v", err)
	}
	// Write should not be done successfully due to flow control.
	msg := make([]byte, initialWindowSize*8)
	err = ct.Write(s, nil, msg, &Options{Last: true, Delay: false})
	expectedErr := streamErrorf(codes.DeadlineExceeded, "%v", context.DeadlineExceeded)
	if err != expectedErr {
		t.Fatalf("Write got %v, want %v", err, expectedErr)
	}
	ct.Close()
	server.stop()
}

func TestMaxStreams(t *testing.T) {
	server, ct := setUp(t, 0, 1, suspended)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Large",
	}
	// Have a pending stream which takes all streams quota.
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	cc, ok := ct.(*http2Client)
	if !ok {
		t.Fatalf("Failed to convert %v to *http2Client", ct)
	}
	done := make(chan struct{})
	ch := make(chan int)
	ready := make(chan struct{})
	go func() {
		for {
			select {
			case <-time.After(5 * time.Millisecond):
				select {
				case ch <- 0:
				case <-ready:
					return
				}
			case <-time.After(5 * time.Second):
				close(done)
				return
			case <-ready:
				return
			}
		}
	}()
	// Test these conditions until they pass or
	// we reach the deadline (failure case).
	for {
		select {
		case <-ch:
		case <-done:
			t.Fatalf("streamsQuota.quota shouldn't be non-zero.")
		}
		cc.streamsQuota.mu.Lock()
		sq := cc.streamsQuota.quota
		cc.streamsQuota.mu.Unlock()
		if sq == 0 {
			break
		}
	}
	close(ready)
	// Close the pending stream so that the streams quota becomes available for the next new stream.
	ct.CloseStream(s, nil)
	cc.streamsQuota.mu.Lock()
	i := cc.streamsQuota.quota
	cc.streamsQuota.mu.Unlock()
	if i != 1 {
		t.Fatalf("streamsQuota is  %d, want 1.", i)
	}
	if _, err := ct.NewStream(context.Background(), callHdr); err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	ct.Close()
	server.stop()
}

func TestServerContextCanceledOnClosedConnection(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, suspended)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo",
	}
	var sc *http2Server
	// Wait until the server transport is setup.
	for {
		server.mu.Lock()
		if len(server.conns) == 0 {
			server.mu.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		for k := range server.conns {
			var ok bool
			sc, ok = k.(*http2Server)
			if !ok {
				t.Fatalf("Failed to convert %v to *http2Server", k)
			}
		}
		server.mu.Unlock()
		break
	}
	cc, ok := ct.(*http2Client)
	if !ok {
		t.Fatalf("Failed to convert %v to *http2Client", ct)
	}
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	cc.controlBuf.put(&dataFrame{s.id, false, make([]byte, http2MaxFrameLen), func() {}})
	// Loop until the server side stream is created.
	var ss *Stream
	for {
		time.Sleep(time.Second)
		sc.mu.Lock()
		if len(sc.activeStreams) == 0 {
			sc.mu.Unlock()
			continue
		}
		ss = sc.activeStreams[s.id]
		sc.mu.Unlock()
		break
	}
	cc.Close()
	select {
	case <-ss.Context().Done():
		if ss.Context().Err() != context.Canceled {
			t.Fatalf("ss.Context().Err() got %v, want %v", ss.Context().Err(), context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Failed to cancel the context of the sever side stream.")
	}
	server.stop()
}

func TestClientConnDecoupledFromApplicationRead(t *testing.T) {
	connectOptions := ConnectOptions{
		InitialWindowSize:     defaultWindowSize,
		InitialConnWindowSize: defaultWindowSize,
	}
	server, client := setUpWithOptions(t, 0, &ServerConfig{}, notifyCall, connectOptions)
	defer server.stop()
	defer client.Close()

	waitWhileTrue(t, func() (bool, error) {
		server.mu.Lock()
		defer server.mu.Unlock()

		if len(server.conns) == 0 {
			return true, fmt.Errorf("timed-out while waiting for connection to be created on the server")
		}
		return false, nil
	})

	var st *http2Server
	server.mu.Lock()
	for k := range server.conns {
		st = k.(*http2Server)
	}
	notifyChan := make(chan struct{})
	server.h.notify = notifyChan
	server.mu.Unlock()
	cstream1, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Client failed to create first stream. Err: %v", err)
	}

	<-notifyChan
	var sstream1 *Stream
	// Access stream on the server.
	st.mu.Lock()
	for _, v := range st.activeStreams {
		if v.id == cstream1.id {
			sstream1 = v
		}
	}
	st.mu.Unlock()
	if sstream1 == nil {
		t.Fatalf("Didn't find stream corresponding to client cstream.id: %v on the server", cstream1.id)
	}
	// Exhaust client's connection window.
	if err := st.Write(sstream1, []byte{}, make([]byte, defaultWindowSize), &Options{}); err != nil {
		t.Fatalf("Server failed to write data. Err: %v", err)
	}
	notifyChan = make(chan struct{})
	server.mu.Lock()
	server.h.notify = notifyChan
	server.mu.Unlock()
	// Create another stream on client.
	cstream2, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Client failed to create second stream. Err: %v", err)
	}
	<-notifyChan
	var sstream2 *Stream
	st.mu.Lock()
	for _, v := range st.activeStreams {
		if v.id == cstream2.id {
			sstream2 = v
		}
	}
	st.mu.Unlock()
	if sstream2 == nil {
		t.Fatalf("Didn't find stream corresponding to client cstream.id: %v on the server", cstream2.id)
	}
	// Server should be able to send data on the new stream, even though the client hasn't read anything on the first stream.
	if err := st.Write(sstream2, []byte{}, make([]byte, defaultWindowSize), &Options{}); err != nil {
		t.Fatalf("Server failed to write data. Err: %v", err)
	}

	// Client should be able to read data on second stream.
	if _, err := cstream2.Read(make([]byte, defaultWindowSize)); err != nil {
		t.Fatalf("_.Read(_) = _, %v, want _, <nil>", err)
	}

	// Client should be able to read data on first stream.
	if _, err := cstream1.Read(make([]byte, defaultWindowSize)); err != nil {
		t.Fatalf("_.Read(_) = _, %v, want _, <nil>", err)
	}
}

func TestServerConnDecoupledFromApplicationRead(t *testing.T) {
	serverConfig := &ServerConfig{
		InitialWindowSize:     defaultWindowSize,
		InitialConnWindowSize: defaultWindowSize,
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, ConnectOptions{})
	defer server.stop()
	defer client.Close()
	waitWhileTrue(t, func() (bool, error) {
		server.mu.Lock()
		defer server.mu.Unlock()

		if len(server.conns) == 0 {
			return true, fmt.Errorf("timed-out while waiting for connection to be created on the server")
		}
		return false, nil
	})
	var st *http2Server
	server.mu.Lock()
	for k := range server.conns {
		st = k.(*http2Server)
	}
	server.mu.Unlock()
	cstream1, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Failed to create 1st stream. Err: %v", err)
	}
	// Exhaust server's connection window.
	if err := client.Write(cstream1, nil, make([]byte, defaultWindowSize), &Options{Last: true}); err != nil {
		t.Fatalf("Client failed to write data. Err: %v", err)
	}
	//Client should be able to create another stream and send data on it.
	cstream2, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Failed to create 2nd stream. Err: %v", err)
	}
	if err := client.Write(cstream2, nil, make([]byte, defaultWindowSize), &Options{}); err != nil {
		t.Fatalf("Client failed to write data. Err: %v", err)
	}
	// Get the streams on server.
	waitWhileTrue(t, func() (bool, error) {
		st.mu.Lock()
		defer st.mu.Unlock()

		if len(st.activeStreams) != 2 {
			return true, fmt.Errorf("timed-out while waiting for server to have created the streams")
		}
		return false, nil
	})
	var sstream1 *Stream
	st.mu.Lock()
	for _, v := range st.activeStreams {
		if v.id == 1 {
			sstream1 = v
		}
	}
	st.mu.Unlock()
	// Trying to write more on a max-ed out stream should result in a RST_STREAM from the server.
	ct := client.(*http2Client)
	ct.controlBuf.put(&dataFrame{cstream2.id, true, make([]byte, 1), func() {}})
	code := http2ErrConvTab[http2.ErrCodeFlowControl]
	waitWhileTrue(t, func() (bool, error) {
		cstream2.mu.Lock()
		defer cstream2.mu.Unlock()
		if cstream2.status.Code() != code {
			return true, fmt.Errorf("want code = %v, got %v", code, cstream2.status.Code())
		}
		return false, nil
	})
	// Reading from the stream on server should succeed.
	if _, err := sstream1.Read(make([]byte, defaultWindowSize)); err != nil {
		t.Fatalf("_.Read(_) = %v, want <nil>", err)
	}

	if _, err := sstream1.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("_.Read(_) = %v, want io.EOF", err)
	}

}

func TestServerWithMisbehavedClient(t *testing.T) {
	serverConfig := &ServerConfig{
		InitialWindowSize:     defaultWindowSize,
		InitialConnWindowSize: defaultWindowSize,
	}
	connectOptions := ConnectOptions{
		InitialWindowSize:     defaultWindowSize,
		InitialConnWindowSize: defaultWindowSize,
	}
	server, ct := setUpWithOptions(t, 0, serverConfig, suspended, connectOptions)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo",
	}
	var sc *http2Server
	// Wait until the server transport is setup.
	for {
		server.mu.Lock()
		if len(server.conns) == 0 {
			server.mu.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		for k := range server.conns {
			var ok bool
			sc, ok = k.(*http2Server)
			if !ok {
				t.Fatalf("Failed to convert %v to *http2Server", k)
			}
		}
		server.mu.Unlock()
		break
	}
	cc, ok := ct.(*http2Client)
	if !ok {
		t.Fatalf("Failed to convert %v to *http2Client", ct)
	}
	// Test server behavior for violation of stream flow control window size restriction.
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	var sent int
	// Drain the stream flow control window
	cc.controlBuf.put(&dataFrame{s.id, false, make([]byte, http2MaxFrameLen), func() {}})
	sent += http2MaxFrameLen
	// Wait until the server creates the corresponding stream and receive some data.
	var ss *Stream
	for {
		time.Sleep(time.Millisecond)
		sc.mu.Lock()
		if len(sc.activeStreams) == 0 {
			sc.mu.Unlock()
			continue
		}
		ss = sc.activeStreams[s.id]
		sc.mu.Unlock()
		ss.fc.mu.Lock()
		if ss.fc.pendingData > 0 {
			ss.fc.mu.Unlock()
			break
		}
		ss.fc.mu.Unlock()
	}
	if ss.fc.pendingData != http2MaxFrameLen || ss.fc.pendingUpdate != 0 || sc.fc.pendingData != 0 || sc.fc.pendingUpdate != 0 {
		t.Fatalf("Server mistakenly updates inbound flow control params: got %d, %d, %d, %d; want %d, %d, %d, %d", ss.fc.pendingData, ss.fc.pendingUpdate, sc.fc.pendingData, sc.fc.pendingUpdate, http2MaxFrameLen, 0, 0, 0)
	}
	// Keep sending until the server inbound window is drained for that stream.
	for sent <= initialWindowSize {
		cc.controlBuf.put(&dataFrame{s.id, false, make([]byte, 1), func() {}})
		sent++
	}
	// Server sent a resetStream for s already.
	code := http2ErrConvTab[http2.ErrCodeFlowControl]
	if _, err := s.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("%v got err %v want <EOF>", s, err)
	}
	if s.status.Code() != code {
		t.Fatalf("%v got status %v; want Code=%v", s, s.status, code)
	}

	ct.CloseStream(s, nil)
	ct.Close()
	server.stop()
}

func TestClientWithMisbehavedServer(t *testing.T) {
	// Turn off BDP estimation so that the server can
	// violate stream window.
	connectOptions := ConnectOptions{
		InitialWindowSize: initialWindowSize,
	}
	server, ct := setUpWithOptions(t, 0, &ServerConfig{}, misbehaved, connectOptions)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo.Stream",
	}
	conn, ok := ct.(*http2Client)
	if !ok {
		t.Fatalf("Failed to convert %v to *http2Client", ct)
	}
	// Test the logic for the violation of stream flow control window size restriction.
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	d := make([]byte, 1)
	if err := ct.Write(s, nil, d, &Options{Last: true, Delay: false}); err != nil && err != io.EOF {
		t.Fatalf("Failed to write: %v", err)
	}
	// Read without window update.
	for {
		p := make([]byte, http2MaxFrameLen)
		if _, err = s.trReader.(*transportReader).reader.Read(p); err != nil {
			break
		}
	}
	if s.fc.pendingData <= initialWindowSize || s.fc.pendingUpdate != 0 || conn.fc.pendingData != 0 || conn.fc.pendingUpdate != 0 {
		t.Fatalf("Client mistakenly updates inbound flow control params: got %d, %d, %d, %d; want >%d, %d, %d, >%d", s.fc.pendingData, s.fc.pendingUpdate, conn.fc.pendingData, conn.fc.pendingUpdate, initialWindowSize, 0, 0, 0)
	}

	if err != io.EOF {
		t.Fatalf("Got err %v, want <EOF>", err)
	}
	if s.status.Code() != codes.Internal {
		t.Fatalf("Got s.status %v, want s.status.Code()=Internal", s.status)
	}

	conn.CloseStream(s, err)
	ct.Close()
	server.stop()
}

var encodingTestStatus = status.New(codes.Internal, "\n")

func TestEncodingRequiredStatus(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, encodingRequiredStatus)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo",
	}
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		return
	}
	opts := Options{
		Last:  true,
		Delay: false,
	}
	if err := ct.Write(s, nil, expectedRequest, &opts); err != nil && err != io.EOF {
		t.Fatalf("Failed to write the request: %v", err)
	}
	p := make([]byte, http2MaxFrameLen)
	if _, err := s.trReader.(*transportReader).Read(p); err != io.EOF {
		t.Fatalf("Read got error %v, want %v", err, io.EOF)
	}
	if !reflect.DeepEqual(s.Status(), encodingTestStatus) {
		t.Fatalf("stream with status %v, want %v", s.Status(), encodingTestStatus)
	}
	ct.Close()
	server.stop()
}

func TestInvalidHeaderField(t *testing.T) {
	server, ct := setUp(t, 0, math.MaxUint32, invalidHeaderField)
	callHdr := &CallHdr{
		Host:   "localhost",
		Method: "foo",
	}
	s, err := ct.NewStream(context.Background(), callHdr)
	if err != nil {
		return
	}
	opts := Options{
		Last:  true,
		Delay: false,
	}
	if err := ct.Write(s, nil, expectedRequest, &opts); err != nil && err != io.EOF {
		t.Fatalf("Failed to write the request: %v", err)
	}
	p := make([]byte, http2MaxFrameLen)
	_, err = s.trReader.(*transportReader).Read(p)
	if se, ok := err.(StreamError); !ok || se.Code != codes.FailedPrecondition || !strings.Contains(err.Error(), expectedInvalidHeaderField) {
		t.Fatalf("Read got error %v, want error with code %s and contains %q", err, codes.FailedPrecondition, expectedInvalidHeaderField)
	}
	ct.Close()
	server.stop()
}

func TestStreamContext(t *testing.T) {
	expectedStream := &Stream{}
	ctx := newContextWithStream(context.Background(), expectedStream)
	s, ok := StreamFromContext(ctx)
	if !ok || expectedStream != s {
		t.Fatalf("GetStreamFromContext(%v) = %v, %t, want: %v, true", ctx, s, ok, expectedStream)
	}
}

func TestIsReservedHeader(t *testing.T) {
	tests := []struct {
		h    string
		want bool
	}{
		{"", false}, // but should be rejected earlier
		{"foo", false},
		{"content-type", true},
		{"grpc-message-type", true},
		{"grpc-encoding", true},
		{"grpc-message", true},
		{"grpc-status", true},
		{"grpc-timeout", true},
		{"te", true},
	}
	for _, tt := range tests {
		got := isReservedHeader(tt.h)
		if got != tt.want {
			t.Errorf("isReservedHeader(%q) = %v; want %v", tt.h, got, tt.want)
		}
	}
}

func TestContextErr(t *testing.T) {
	for _, test := range []struct {
		// input
		errIn error
		// outputs
		errOut StreamError
	}{
		{context.DeadlineExceeded, StreamError{codes.DeadlineExceeded, context.DeadlineExceeded.Error()}},
		{context.Canceled, StreamError{codes.Canceled, context.Canceled.Error()}},
	} {
		err := ContextErr(test.errIn)
		if err != test.errOut {
			t.Fatalf("ContextErr{%v} = %v \nwant %v", test.errIn, err, test.errOut)
		}
	}
}

func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

type windowSizeConfig struct {
	serverStream int32
	serverConn   int32
	clientStream int32
	clientConn   int32
}

func TestAccountCheckWindowSizeWithLargeWindow(t *testing.T) {
	wc := windowSizeConfig{
		serverStream: 10 * 1024 * 1024,
		serverConn:   12 * 1024 * 1024,
		clientStream: 6 * 1024 * 1024,
		clientConn:   8 * 1024 * 1024,
	}
	testAccountCheckWindowSize(t, wc)
}

func TestAccountCheckWindowSizeWithSmallWindow(t *testing.T) {
	wc := windowSizeConfig{
		serverStream: defaultWindowSize,
		// Note this is smaller than initialConnWindowSize which is the current default.
		serverConn:   defaultWindowSize,
		clientStream: defaultWindowSize,
		clientConn:   defaultWindowSize,
	}
	testAccountCheckWindowSize(t, wc)
}

func testAccountCheckWindowSize(t *testing.T, wc windowSizeConfig) {
	serverConfig := &ServerConfig{
		InitialWindowSize:     wc.serverStream,
		InitialConnWindowSize: wc.serverConn,
	}
	connectOptions := ConnectOptions{
		InitialWindowSize:     wc.clientStream,
		InitialConnWindowSize: wc.clientConn,
	}
	server, client := setUpWithOptions(t, 0, serverConfig, suspended, connectOptions)
	defer server.stop()
	defer client.Close()

	// Wait for server conns to be populated with new server transport.
	waitWhileTrue(t, func() (bool, error) {
		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.conns) == 0 {
			return true, fmt.Errorf("timed out waiting for server transport to be created")
		}
		return false, nil
	})
	var st *http2Server
	server.mu.Lock()
	for k := range server.conns {
		st = k.(*http2Server)
	}
	server.mu.Unlock()
	ct := client.(*http2Client)
	cstream, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Failed to create stream. Err: %v", err)
	}
	// Wait for server to receive headers.
	waitWhileTrue(t, func() (bool, error) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if len(st.activeStreams) == 0 {
			return true, fmt.Errorf("timed out waiting for server to receive headers")
		}
		return false, nil
	})
	// Sleeping to make sure the settings are applied in case of negative test.
	time.Sleep(time.Second)

	waitWhileTrue(t, func() (bool, error) {
		st.fc.mu.Lock()
		lim := st.fc.limit
		st.fc.mu.Unlock()
		if lim != uint32(serverConfig.InitialConnWindowSize) {
			return true, fmt.Errorf("Server transport flow control window size: got %v, want %v", lim, serverConfig.InitialConnWindowSize)
		}
		return false, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	serverSendQuota, _, err := st.sendQuotaPool.get(math.MaxInt32, waiters{
		ctx:    ctx,
		tctx:   st.ctx,
		done:   nil,
		goAway: nil,
	})
	if err != nil {
		t.Fatalf("Error while acquiring sendQuota on server. Err: %v", err)
	}
	cancel()
	st.sendQuotaPool.add(serverSendQuota)
	if serverSendQuota != int(connectOptions.InitialConnWindowSize) {
		t.Fatalf("Server send quota(%v) not equal to client's window size(%v) on conn.", serverSendQuota, connectOptions.InitialConnWindowSize)
	}
	st.mu.Lock()
	ssq := st.streamSendQuota
	st.mu.Unlock()
	if ssq != uint32(connectOptions.InitialWindowSize) {
		t.Fatalf("Server stream send quota(%v) not equal to client's window size(%v) on stream.", ssq, connectOptions.InitialWindowSize)
	}
	ct.fc.mu.Lock()
	limit := ct.fc.limit
	ct.fc.mu.Unlock()
	if limit != uint32(connectOptions.InitialConnWindowSize) {
		t.Fatalf("Client transport flow control window size is %v, want %v", limit, connectOptions.InitialConnWindowSize)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	clientSendQuota, _, err := ct.sendQuotaPool.get(math.MaxInt32, waiters{
		ctx:    ctx,
		tctx:   ct.ctx,
		done:   nil,
		goAway: nil,
	})
	if err != nil {
		t.Fatalf("Error while acquiring sendQuota on client. Err: %v", err)
	}
	cancel()
	ct.sendQuotaPool.add(clientSendQuota)
	if clientSendQuota != int(serverConfig.InitialConnWindowSize) {
		t.Fatalf("Client send quota(%v) not equal to server's window size(%v) on conn.", clientSendQuota, serverConfig.InitialConnWindowSize)
	}
	ct.mu.Lock()
	ssq = ct.streamSendQuota
	ct.mu.Unlock()
	if ssq != uint32(serverConfig.InitialWindowSize) {
		t.Fatalf("Client stream send quota(%v) not equal to server's window size(%v) on stream.", ssq, serverConfig.InitialWindowSize)
	}
	cstream.fc.mu.Lock()
	limit = cstream.fc.limit
	cstream.fc.mu.Unlock()
	if limit != uint32(connectOptions.InitialWindowSize) {
		t.Fatalf("Client stream flow control window size is %v, want %v", limit, connectOptions.InitialWindowSize)
	}
	var sstream *Stream
	st.mu.Lock()
	for _, v := range st.activeStreams {
		sstream = v
	}
	st.mu.Unlock()
	sstream.fc.mu.Lock()
	limit = sstream.fc.limit
	sstream.fc.mu.Unlock()
	if limit != uint32(serverConfig.InitialWindowSize) {
		t.Fatalf("Server stream flow control window size is %v, want %v", limit, serverConfig.InitialWindowSize)
	}
}

// Check accounting on both sides after sending and receiving large messages.
func TestAccountCheckExpandingWindow(t *testing.T) {
	server, client := setUp(t, 0, 0, pingpong)
	defer server.stop()
	defer client.Close()
	waitWhileTrue(t, func() (bool, error) {
		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.conns) == 0 {
			return true, fmt.Errorf("timed out while waiting for server transport to be created")
		}
		return false, nil
	})
	var st *http2Server
	server.mu.Lock()
	for k := range server.conns {
		st = k.(*http2Server)
	}
	server.mu.Unlock()
	ct := client.(*http2Client)
	cstream, err := client.NewStream(context.Background(), &CallHdr{Flush: true})
	if err != nil {
		t.Fatalf("Failed to create stream. Err: %v", err)
	}

	msgSize := 65535 * 16 * 2
	msg := make([]byte, msgSize)
	buf := make([]byte, msgSize+5)
	buf[0] = byte(0)
	binary.BigEndian.PutUint32(buf[1:], uint32(msgSize))
	copy(buf[5:], msg)
	opts := Options{}
	header := make([]byte, 5)
	for i := 1; i <= 10; i++ {
		if err := ct.Write(cstream, nil, buf, &opts); err != nil {
			t.Fatalf("Error on client while writing message: %v", err)
		}
		if _, err := cstream.Read(header); err != nil {
			t.Fatalf("Error on client while reading data frame header: %v", err)
		}
		sz := binary.BigEndian.Uint32(header[1:])
		recvMsg := make([]byte, int(sz))
		if _, err := cstream.Read(recvMsg); err != nil {
			t.Fatalf("Error on client while reading data: %v", err)
		}
		if len(recvMsg) != len(msg) {
			t.Fatalf("Length of message received by client: %v, want: %v", len(recvMsg), len(msg))
		}
	}
	defer func() {
		ct.Write(cstream, nil, nil, &Options{Last: true}) // Close the stream.
		if _, err := cstream.Read(header); err != io.EOF {
			t.Fatalf("Client expected an EOF from the server. Got: %v", err)
		}
	}()
	var sstream *Stream
	st.mu.Lock()
	for _, v := range st.activeStreams {
		sstream = v
	}
	st.mu.Unlock()

	waitWhileTrue(t, func() (bool, error) {
		// Check that pendingData and delta on flow control windows on both sides are 0.
		cstream.fc.mu.Lock()
		if cstream.fc.delta != 0 {
			cstream.fc.mu.Unlock()
			return true, fmt.Errorf("delta on flow control window of client stream is non-zero")
		}
		if cstream.fc.pendingData != 0 {
			cstream.fc.mu.Unlock()
			return true, fmt.Errorf("pendingData on flow control window of client stream is non-zero")
		}
		cstream.fc.mu.Unlock()
		sstream.fc.mu.Lock()
		if sstream.fc.delta != 0 {
			sstream.fc.mu.Unlock()
			return true, fmt.Errorf("delta on flow control window of server stream is non-zero")
		}
		if sstream.fc.pendingData != 0 {
			sstream.fc.mu.Unlock()
			return true, fmt.Errorf("pendingData on flow control window of sercer stream is non-zero")
		}
		sstream.fc.mu.Unlock()
		ct.fc.mu.Lock()
		if ct.fc.delta != 0 {
			ct.fc.mu.Unlock()
			return true, fmt.Errorf("delta on flow control window of client transport is non-zero")
		}
		if ct.fc.pendingData != 0 {
			ct.fc.mu.Unlock()
			return true, fmt.Errorf("pendingData on flow control window of client transport is non-zero")
		}
		ct.fc.mu.Unlock()
		st.fc.mu.Lock()
		if st.fc.delta != 0 {
			st.fc.mu.Unlock()
			return true, fmt.Errorf("delta on flow control window of server transport is non-zero")
		}
		if st.fc.pendingData != 0 {
			st.fc.mu.Unlock()
			return true, fmt.Errorf("pendingData on flow control window of server transport is non-zero")
		}
		st.fc.mu.Unlock()

		// Check flow conrtrol window on client stream is equal to out flow on server stream.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		serverStreamSendQuota, _, err := sstream.sendQuotaPool.get(math.MaxInt32, waiters{
			ctx:    ctx,
			tctx:   context.Background(),
			done:   nil,
			goAway: nil,
		})
		cancel()
		if err != nil {
			return true, fmt.Errorf("error while acquiring server stream send quota. Err: %v", err)
		}
		sstream.sendQuotaPool.add(serverStreamSendQuota)
		cstream.fc.mu.Lock()
		clientEst := cstream.fc.limit - cstream.fc.pendingUpdate
		cstream.fc.mu.Unlock()
		if uint32(serverStreamSendQuota) != clientEst {
			return true, fmt.Errorf("server stream outflow: %v, estimated by client: %v", serverStreamSendQuota, clientEst)
		}

		// Check flow control window on server stream is equal to out flow on client stream.
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		clientStreamSendQuota, _, err := cstream.sendQuotaPool.get(math.MaxInt32, waiters{
			ctx:    ctx,
			tctx:   context.Background(),
			done:   nil,
			goAway: nil,
		})
		cancel()
		if err != nil {
			return true, fmt.Errorf("error while acquiring client stream send quota. Err: %v", err)
		}
		cstream.sendQuotaPool.add(clientStreamSendQuota)
		sstream.fc.mu.Lock()
		serverEst := sstream.fc.limit - sstream.fc.pendingUpdate
		sstream.fc.mu.Unlock()
		if uint32(clientStreamSendQuota) != serverEst {
			return true, fmt.Errorf("client stream outflow: %v. estimated by server: %v", clientStreamSendQuota, serverEst)
		}

		// Check flow control window on client transport is equal to out flow of server transport.
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		serverTrSendQuota, _, err := st.sendQuotaPool.get(math.MaxInt32, waiters{
			ctx:    ctx,
			tctx:   st.ctx,
			done:   nil,
			goAway: nil,
		})
		cancel()
		if err != nil {
			return true, fmt.Errorf("error while acquring server transport send quota. Err: %v", err)
		}
		st.sendQuotaPool.add(serverTrSendQuota)
		ct.fc.mu.Lock()
		clientEst = ct.fc.limit - ct.fc.pendingUpdate
		ct.fc.mu.Unlock()
		if uint32(serverTrSendQuota) != clientEst {
			return true, fmt.Errorf("server transport outflow: %v, estimated by client: %v", serverTrSendQuota, clientEst)
		}

		// Check flow control window on server transport is equal to out flow of client transport.
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		clientTrSendQuota, _, err := ct.sendQuotaPool.get(math.MaxInt32, waiters{
			ctx:    ctx,
			tctx:   ct.ctx,
			done:   nil,
			goAway: nil,
		})
		cancel()
		if err != nil {
			return true, fmt.Errorf("error while acquiring client transport send quota. Err: %v", err)
		}
		ct.sendQuotaPool.add(clientTrSendQuota)
		st.fc.mu.Lock()
		serverEst = st.fc.limit - st.fc.pendingUpdate
		st.fc.mu.Unlock()
		if uint32(clientTrSendQuota) != serverEst {
			return true, fmt.Errorf("client transport outflow: %v, estimated by client: %v", clientTrSendQuota, serverEst)
		}

		return false, nil
	})

}

func waitWhileTrue(t *testing.T, condition func() (bool, error)) {
	var (
		wait bool
		err  error
	)
	timer := time.NewTimer(time.Second * 5)
	for {
		wait, err = condition()
		if wait {
			select {
			case <-timer.C:
				t.Fatalf(err.Error())
			default:
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}
		if !timer.Stop() {
			<-timer.C
		}
		break
	}
}

// A function of type writeHeaders writes out
// http status with the given stream ID using the given framer.
type writeHeaders func(*http2.Framer, uint32, int) error

func writeOneHeader(framer *http2.Framer, sid uint32, httpStatus int) error {
	var buf bytes.Buffer
	henc := hpack.NewEncoder(&buf)
	henc.WriteField(hpack.HeaderField{Name: ":status", Value: fmt.Sprint(httpStatus)})
	return framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      sid,
		BlockFragment: buf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
}

func writeTwoHeaders(framer *http2.Framer, sid uint32, httpStatus int) error {
	var buf bytes.Buffer
	henc := hpack.NewEncoder(&buf)
	henc.WriteField(hpack.HeaderField{
		Name:  ":status",
		Value: fmt.Sprint(http.StatusOK),
	})
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      sid,
		BlockFragment: buf.Bytes(),
		EndHeaders:    true,
	}); err != nil {
		return err
	}
	buf.Reset()
	henc.WriteField(hpack.HeaderField{
		Name:  ":status",
		Value: fmt.Sprint(httpStatus),
	})
	return framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      sid,
		BlockFragment: buf.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
}

type httpServer struct {
	conn       net.Conn
	httpStatus int
	wh         writeHeaders
}

func (s *httpServer) start(t *testing.T, lis net.Listener) {
	// Launch an HTTP server to send back header with httpStatus.
	go func() {
		var err error
		s.conn, err = lis.Accept()
		if err != nil {
			t.Errorf("Error accepting connection: %v", err)
			return
		}
		defer s.conn.Close()
		// Read preface sent by client.
		if _, err = io.ReadFull(s.conn, make([]byte, len(http2.ClientPreface))); err != nil {
			t.Errorf("Error at server-side while reading preface from cleint. Err: %v", err)
			return
		}
		reader := bufio.NewReaderSize(s.conn, defaultWriteBufSize)
		writer := bufio.NewWriterSize(s.conn, defaultReadBufSize)
		framer := http2.NewFramer(writer, reader)
		if err = framer.WriteSettingsAck(); err != nil {
			t.Errorf("Error at server-side while sending Settings ack. Err: %v", err)
			return
		}
		var sid uint32
		// Read frames until a header is received.
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				t.Errorf("Error at server-side while reading frame. Err: %v", err)
				return
			}
			if hframe, ok := frame.(*http2.HeadersFrame); ok {
				sid = hframe.Header().StreamID
				break
			}
		}
		if err = s.wh(framer, sid, s.httpStatus); err != nil {
			t.Errorf("Error at server-side while writing headers. Err: %v", err)
			return
		}
		writer.Flush()
	}()
}

func (s *httpServer) cleanUp() {
	if s.conn != nil {
		s.conn.Close()
	}
}

func setUpHTTPStatusTest(t *testing.T, httpStatus int, wh writeHeaders) (stream *Stream, cleanUp func()) {
	var (
		err    error
		lis    net.Listener
		server *httpServer
		client ClientTransport
	)
	cleanUp = func() {
		if lis != nil {
			lis.Close()
		}
		if server != nil {
			server.cleanUp()
		}
		if client != nil {
			client.Close()
		}
	}
	defer func() {
		if err != nil {
			cleanUp()
		}
	}()
	lis, err = net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen. Err: %v", err)
	}
	server = &httpServer{
		httpStatus: httpStatus,
		wh:         wh,
	}
	server.start(t, lis)
	connectCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	client, err = newHTTP2Client(connectCtx, context.Background(), TargetInfo{Addr: lis.Addr().String()}, ConnectOptions{}, func() {})
	if err != nil {
		cancel() // Do not cancel in success path.
		t.Fatalf("Error creating client. Err: %v", err)
	}
	stream, err = client.NewStream(context.Background(), &CallHdr{Method: "bogus/method", Flush: true})
	if err != nil {
		t.Fatalf("Error creating stream at client-side. Err: %v", err)
	}
	return
}

func TestHTTPToGRPCStatusMapping(t *testing.T) {
	for k := range httpStatusConvTab {
		testHTTPToGRPCStatusMapping(t, k, writeOneHeader)
	}
}

func testHTTPToGRPCStatusMapping(t *testing.T, httpStatus int, wh writeHeaders) {
	stream, cleanUp := setUpHTTPStatusTest(t, httpStatus, wh)
	defer cleanUp()
	want := httpStatusConvTab[httpStatus]
	buf := make([]byte, 8)
	_, err := stream.Read(buf)
	if err == nil {
		t.Fatalf("Stream.Read(_) unexpectedly returned no error. Expected stream error with code %v", want)
	}
	serr, ok := err.(StreamError)
	if !ok {
		t.Fatalf("err.(Type) = %T, want StreamError", err)
	}
	if want != serr.Code {
		t.Fatalf("Want error code: %v, got: %v", want, serr.Code)
	}
}

func TestHTTPStatusOKAndMissingGRPCStatus(t *testing.T) {
	stream, cleanUp := setUpHTTPStatusTest(t, http.StatusOK, writeOneHeader)
	defer cleanUp()
	buf := make([]byte, 8)
	_, err := stream.Read(buf)
	if err != io.EOF {
		t.Fatalf("stream.Read(_) = _, %v, want _, io.EOF", err)
	}
	want := codes.Unknown
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.status.Code() != want {
		t.Fatalf("Status code of stream: %v, want: %v", stream.status.Code(), want)
	}
}

func TestHTTPStatusNottOKAndMissingGRPCStatusInSecondHeader(t *testing.T) {
	testHTTPToGRPCStatusMapping(t, http.StatusUnauthorized, writeTwoHeaders)
}

// If any error occurs on a call to Stream.Read, future calls
// should continue to return that same error.
func TestReadGivesSameErrorAfterAnyErrorOccurs(t *testing.T) {
	testRecvBuffer := newRecvBuffer()
	s := &Stream{
		ctx:         context.Background(),
		goAway:      make(chan struct{}),
		buf:         testRecvBuffer,
		requestRead: func(int) {},
	}
	s.trReader = &transportReader{
		reader: &recvBufferReader{
			ctx:    s.ctx,
			goAway: s.goAway,
			recv:   s.buf,
		},
		windowHandler: func(int) {},
	}
	testData := make([]byte, 1)
	testData[0] = 5
	testErr := errors.New("test error")
	s.write(recvMsg{data: testData, err: testErr})

	inBuf := make([]byte, 1)
	actualCount, actualErr := s.Read(inBuf)
	if actualCount != 0 {
		t.Errorf("actualCount, _ := s.Read(_) differs; want 0; got %v", actualCount)
	}
	if actualErr.Error() != testErr.Error() {
		t.Errorf("_ , actualErr := s.Read(_) differs; want actualErr.Error() to be %v; got %v", testErr.Error(), actualErr.Error())
	}

	s.write(recvMsg{data: testData, err: nil})
	s.write(recvMsg{data: testData, err: errors.New("different error from first")})

	for i := 0; i < 2; i++ {
		inBuf := make([]byte, 1)
		actualCount, actualErr := s.Read(inBuf)
		if actualCount != 0 {
			t.Errorf("actualCount, _ := s.Read(_) differs; want %v; got %v", 0, actualCount)
		}
		if actualErr.Error() != testErr.Error() {
			t.Errorf("_ , actualErr := s.Read(_) differs; want actualErr.Error() to be %v; got %v", testErr.Error(), actualErr.Error())
		}
	}
}

func TestPingPong1B(t *testing.T) {
	runPingPongTest(t, 1)
}

func TestPingPong1KB(t *testing.T) {
	runPingPongTest(t, 1024)
}

func TestPingPong64KB(t *testing.T) {
	runPingPongTest(t, 65536)
}

func TestPingPong1MB(t *testing.T) {
	runPingPongTest(t, 1048576)
}

//This is a stress-test of flow control logic.
func runPingPongTest(t *testing.T, msgSize int) {
	server, client := setUp(t, 0, 0, pingpong)
	defer server.stop()
	defer client.Close()
	waitWhileTrue(t, func() (bool, error) {
		server.mu.Lock()
		defer server.mu.Unlock()
		if len(server.conns) == 0 {
			return true, fmt.Errorf("timed out while waiting for server transport to be created")
		}
		return false, nil
	})
	ct := client.(*http2Client)
	stream, err := client.NewStream(context.Background(), &CallHdr{})
	if err != nil {
		t.Fatalf("Failed to create stream. Err: %v", err)
	}
	msg := make([]byte, msgSize)
	outgoingHeader := make([]byte, 5)
	outgoingHeader[0] = byte(0)
	binary.BigEndian.PutUint32(outgoingHeader[1:], uint32(msgSize))
	opts := &Options{}
	incomingHeader := make([]byte, 5)
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(time.Second * 5)
		<-timer.C
		close(done)
	}()
	for {
		select {
		case <-done:
			ct.Write(stream, nil, nil, &Options{Last: true})
			if _, err := stream.Read(incomingHeader); err != io.EOF {
				t.Fatalf("Client expected EOF from the server. Got: %v", err)
			}
			return
		default:
			if err := ct.Write(stream, outgoingHeader, msg, opts); err != nil {
				t.Fatalf("Error on client while writing message. Err: %v", err)
			}
			if _, err := stream.Read(incomingHeader); err != nil {
				t.Fatalf("Error on client while reading data header. Err: %v", err)
			}
			sz := binary.BigEndian.Uint32(incomingHeader[1:])
			recvMsg := make([]byte, int(sz))
			if _, err := stream.Read(recvMsg); err != nil {
				t.Fatalf("Error on client while reading data. Err: %v", err)
			}
		}
	}
}
