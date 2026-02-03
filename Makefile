.PHONY: test fmt lint coverage clean help

help:
	@echo "Lokutor Voice Agent - Go Orchestrator"
	@echo ""
	@echo "Available targets:"
	@echo "  test     - Run all tests with verbose output"
	@echo "  coverage - Run tests and generate coverage report"
	@echo "  fmt      - Format code with gofmt"
	@echo "  lint     - Run go vet"
	@echo "  clean    - Clean build artifacts"
	@echo "  help     - Show this help message"

test:
	go test -v -race ./...

coverage:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

fmt:
	go fmt ./...
	@echo "Code formatted"

lint:
	go vet ./...
	@echo "Linting complete"

clean:
	rm -f coverage.out coverage.html
	go clean
	@echo "Clean complete"
