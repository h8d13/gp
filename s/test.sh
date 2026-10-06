#!/bin/sh
# Static checks, then the git-style shell suite. Hermetic: e2e.sh drives the
# binary against local servers.
go vet ./... && sh t/e2e.sh
