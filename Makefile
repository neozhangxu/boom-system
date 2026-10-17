.PHONY: build

build:
	npm install --no-audit --no-fund
	npm run build
	mkdir -p dist
	go build -o dist/boom-system .
