sign:
	codesign --force --sign "UDL" -i udl ~/bin/udl

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1
