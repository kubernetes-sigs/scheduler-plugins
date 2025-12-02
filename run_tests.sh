# run_tests.sh
#!/usr/bin/env bash
set -euo pipefail

mkdir -p coverage/python
mkdir -p coverage/go

pytest
go test ./pkg/mypriorityoptimizer -coverprofile=coverage/go/go_coverage.out
go tool cover -func=coverage/go/go_coverage.out
go tool cover -html=coverage/go/go_coverage.out -o coverage/go/coverage.html

echo "Coverage reports generated in coverage/python and coverage/go directories."