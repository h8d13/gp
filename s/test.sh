#!/bin/sh
# Static checks, then the git-style shell suites. e2e.sh is hermetic;
# parity.sh's network tests skip unless GP_NET=1.
go vet ./... && sh t/e2e.sh && sh t/parity.sh
