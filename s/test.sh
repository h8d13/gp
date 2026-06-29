#!/bin/sh
# Static checks, then the git-style shell suites. Both are hermetic: e2e.sh
# drives the binary against local servers, srcs-e2e.sh lints sources.ini via
# `gp up check`.
go vet ./... && sh t/e2e.sh && sh t/srcs-e2e.sh
