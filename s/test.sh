#!/bin/sh
# Static checks, then the git-style shell suites. e2e.sh is hermetic
go vet ./... && sh t/e2e.sh
