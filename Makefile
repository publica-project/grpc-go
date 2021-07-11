all: test testrace

deps:
	go get -d -v github.com/publica-project/grpc/...

updatedeps:
	go get -d -v -u -f github.com/publica-project/grpc/...

testdeps:
	go get -d -v -t github.com/publica-project/grpc/...

updatetestdeps:
	go get -d -v -t -u -f github.com/publica-project/grpc/...

build: deps
	go build github.com/publica-project/grpc/...

proto:
	@ if ! which protoc > /dev/null; then \
		echo "error: protoc not installed" >&2; \
		exit 1; \
	fi
	go generate github.com/publica-project/grpc/...

test: testdeps
	go test -cpu 1,4 -timeout 5m github.com/publica-project/grpc/...

testrace: testdeps
	go test -race -cpu 1,4 -timeout 7m github.com/publica-project/grpc/...

clean:
	go clean -i github.com/publica-project/grpc/...

.PHONY: \
	all \
	deps \
	updatedeps \
	testdeps \
	updatetestdeps \
	build \
	proto \
	test \
	testrace \
	clean \
	coverage
