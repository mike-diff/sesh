#!/bin/sh
# $1 is the task directory. Pass: the hidden cases pass with the agent's own tests.
set -e
cp "$1/hidden/hidden_test.go" .
go test ./...
