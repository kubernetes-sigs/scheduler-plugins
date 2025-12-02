# scripts/go_coverage.sh
#!/usr/bin/env bash
set -euo pipefail

mkdir -p coverage/go

go test ./pkg/mypriorityoptimizer -coverprofile=coverage/go/go_coverage.out

go tool cover -func=coverage/go/go_coverage.out

go tool cover -html=coverage/go/go_coverage.out -o coverage/go/coverage.html

echo "HTML coverage report: coverage/go/coverage.html"