// +build go1.6,!go1.7

/*
 *
 * Copyright 2016 gRPC authors.
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
	"net"
	"net/http"

	"github.com/publica-project/grpc/codes"

	"golang.org/x/net/context"
)

// dialContext connects to the address on the named network.
func dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{Cancel: ctx.Done()}).Dial(network, address)
}

// ContextErr converts the error from context package into a StreamError.
func ContextErr(err error) StreamError {
	switch err {
	case context.DeadlineExceeded:
		return streamErrorf(codes.DeadlineExceeded, "%v", err)
	case context.Canceled:
		return streamErrorf(codes.Canceled, "%v", err)
	}
	return streamErrorf(codes.Internal, "Unexpected error from context packet: %v", err)
}

// contextFromRequest returns a background context.
func contextFromRequest(r *http.Request) context.Context {
	return context.Background()
}
