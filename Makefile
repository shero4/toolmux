.PHONY: css test run

css:
	npm run build:css

test:
	npm run build:css
	go test ./...

run:
	npm run build:css
	go run ./cmd/toolmux
