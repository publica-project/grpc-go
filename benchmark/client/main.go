/*
 *
 * Copyright 2017 gRPC authors.
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

package main

import (
	"flag"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof"
	"sync"
	"time"

	"github.com/publica-project/grpc"
	"github.com/publica-project/grpc/benchmark"
	testpb "github.com/publica-project/grpc/benchmark/grpc_testing"
	"github.com/publica-project/grpc/benchmark/stats"
	"github.com/publica-project/grpc/grpclog"
	"golang.org/x/net/context"
)

var (
	server            = flag.String("server", "", "The server address")
	maxConcurrentRPCs = flag.Int("max_concurrent_rpcs", 1, "The max number of concurrent RPCs")
	duration          = flag.Int("duration", math.MaxInt32, "The duration in seconds to run the benchmark client")
	trace             = flag.Bool("trace", true, "Whether tracing is on")
	rpcType           = flag.Int("rpc_type", 0,
		`Configure different client rpc type. Valid options are:
		   0 : unary call;
		   1 : streaming call.`)
)

func unaryCaller(client testpb.BenchmarkServiceClient) {
	benchmark.DoUnaryCall(client, 1, 1)
}

func streamCaller(stream testpb.BenchmarkService_StreamingCallClient) {
	benchmark.DoStreamingRoundTrip(stream, 1, 1)
}

func buildConnection() (s *stats.Stats, conn *grpc.ClientConn, tc testpb.BenchmarkServiceClient) {
	s = stats.NewStats(256)
	conn = benchmark.NewClientConn(*server)
	tc = testpb.NewBenchmarkServiceClient(conn)
	return s, conn, tc
}

func closeLoopUnary() {
	s, conn, tc := buildConnection()

	for i := 0; i < 100; i++ {
		unaryCaller(tc)
	}
	ch := make(chan int, *maxConcurrentRPCs*4)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	wg.Add(*maxConcurrentRPCs)

	for i := 0; i < *maxConcurrentRPCs; i++ {
		go func() {
			for range ch {
				start := time.Now()
				unaryCaller(tc)
				elapse := time.Since(start)
				mu.Lock()
				s.Add(elapse)
				mu.Unlock()
			}
			wg.Done()
		}()
	}
	// Stop the client when time is up.
	done := make(chan struct{})
	go func() {
		<-time.After(time.Duration(*duration) * time.Second)
		close(done)
	}()
	ok := true
	for ok {
		select {
		case ch <- 0:
		case <-done:
			ok = false
		}
	}
	close(ch)
	wg.Wait()
	conn.Close()
	grpclog.Println(s.String())

}

func closeLoopStream() {
	s, conn, tc := buildConnection()
	ch := make(chan int, *maxConcurrentRPCs*4)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	wg.Add(*maxConcurrentRPCs)
	// Distribute RPCs over maxConcurrentCalls workers.
	for i := 0; i < *maxConcurrentRPCs; i++ {
		go func() {
			stream, err := tc.StreamingCall(context.Background())
			if err != nil {
				grpclog.Fatalf("%v.StreamingCall(_) = _, %v", tc, err)
			}
			// Do some warm up.
			for i := 0; i < 100; i++ {
				streamCaller(stream)
			}
			for range ch {
				start := time.Now()
				streamCaller(stream)
				elapse := time.Since(start)
				mu.Lock()
				s.Add(elapse)
				mu.Unlock()
			}
			wg.Done()
		}()
	}
	// Stop the client when time is up.
	done := make(chan struct{})
	go func() {
		<-time.After(time.Duration(*duration) * time.Second)
		close(done)
	}()
	ok := true
	for ok {
		select {
		case ch <- 0:
		case <-done:
			ok = false
		}
	}
	close(ch)
	wg.Wait()
	conn.Close()
	grpclog.Println(s.String())
}

func main() {
	flag.Parse()
	grpc.EnableTracing = *trace
	go func() {
		lis, err := net.Listen("tcp", ":0")
		if err != nil {
			grpclog.Fatalf("Failed to listen: %v", err)
		}
		grpclog.Println("Client profiling address: ", lis.Addr().String())
		if err := http.Serve(lis, nil); err != nil {
			grpclog.Fatalf("Failed to serve: %v", err)
		}
	}()
	switch *rpcType {
	case 0:
		closeLoopUnary()
	case 1:
		closeLoopStream()
	}
}
