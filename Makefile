
test:
	FAILURE_FLAGS_ENABLED=1 go tool gotestsum --junitfile /tmp/failure-flags-go.junit.xml

vet:
	go vet ./...

fmt:
	go fmt ./...
