# GitHub Configuration Files

## .gitignore
```
# Binaries for programs and plugins
*.exe
*.exe~
*.dll
*.so
*.so.*
*.dylib

# Test binary, built with `go test -c`
*.test

# Output of the go coverage tool
*.out

# Dependency directories
vendor/

# Go workspace file
go.work

# IDE
.idea/
.vscode/
*.swp
*.swo
*~
.DS_Store

# Test coverage
coverage.out
coverage.html
```

## GitHub Actions Workflow (.github/workflows/test.yml)
```yaml
name: Tests

on:
  push:
    branches: [ main, develop ]
  pull_request:
    branches: [ main, develop ]

jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        go-version: [1.20, 1.21, 1.22]
    steps:
    - uses: actions/checkout@v3
    - name: Set up Go
      uses: actions/setup-go@v4
      with:
        go-version: ${{ matrix.go-version }}
    - name: Run tests
      run: go test -v -race -coverprofile=coverage.out ./...
    - name: Upload coverage
      uses: codecov/codecov-action@v3
      with:
        files: ./coverage.out
```

## Makefile
```makefile
.PHONY: test fmt lint clean help

help:
	@echo "Lokutor Voice Agent - Go Orchestrator"
	@echo ""
	@echo "Available targets:"
	@echo "  test     - Run all tests"
	@echo "  fmt      - Format code"
	@echo "  lint     - Run linter"
	@echo "  clean    - Clean build artifacts"

test:
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

fmt:
	go fmt ./...

lint:
	go vet ./...

clean:
	rm -f coverage.out coverage.html
	go clean
```
