.PHONY: css build release test run
css:
	tools/tailwindcss -c tailwind.config.js -i internal/web/input.css -o internal/web/static/app.css --minify
build: css
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/lastseen ./cmd/lastseen
# Static Linux binary for the VPS.
release: css
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/lastseen-linux-amd64 ./cmd/lastseen
test:
	go vet ./... && go test ./...
run: css
	go run ./cmd/lastseen serve
