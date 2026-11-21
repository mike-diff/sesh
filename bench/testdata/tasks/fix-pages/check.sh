#!/bin/sh
# $1 is the task directory. Pass: the original test is untouched and the
# hidden cases pass alongside it.
set -e
cmp -s pages_test.go "$1/repo/pages_test.go" || { echo "pages_test.go was modified"; exit 1; }
cp "$1/hidden/hidden_test.go" .
go test ./...
