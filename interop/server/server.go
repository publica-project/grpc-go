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

package main

import (
	"flag"
	"net"
	"strconv"

	"github.com/publica-project/grpc"
	"github.com/publica-project/grpc/credentials"
	"github.com/publica-project/grpc/grpclog"
	"github.com/publica-project/grpc/interop"
	testpb "github.com/publica-project/grpc/interop/grpc_testing"
	"github.com/publica-project/grpc/testdata"
)

var (
	useTLS   = flag.Bool("use_tls", false, "Connection uses TLS if true, else plain TCP")
	certFile = flag.String("tls_cert_file", "", "The TLS cert file")
	keyFile  = flag.String("tls_key_file", "", "The TLS key file")
	port     = flag.Int("port", 10000, "The server port")
)

func main() {
	flag.Parse()
	p := strconv.Itoa(*port)
	lis, err := net.Listen("tcp", ":"+p)
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	if *useTLS {
		if *certFile == "" {
			*certFile = testdata.Path("server1.pem")
		}
		if *keyFile == "" {
			*keyFile = testdata.Path("server1.key")
		}
		creds, err := credentials.NewServerTLSFromFile(*certFile, *keyFile)
		if err != nil {
			grpclog.Fatalf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	}
	server := grpc.NewServer(opts...)
	testpb.RegisterTestServiceServer(server, interop.NewTestServer())
	server.Serve(lis)
}
