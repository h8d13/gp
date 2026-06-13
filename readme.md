# gp

`get-packets` is a small `go` lib that speaks everything `http(s)`

---
<!-- gp-help:begin (generated; edit flags or gen-readme.sh, not below) -->

## Usage

```
Usage of gp:
  -o, --output string
    	save response body to file
  -p, --parallel int
    	parallel connections, overrides config; 1 disables (needs -o)
  -q, --quic
    	use HTTP/3 over QUIC, overrides config
  -r, --retries int
    	retries on 429/503, overrides config; 0 disables
```

## Configuration

Keys resolve in order: real env > .env > ini > default. See gpconfig.ini for
the annotated sample (XDG path: $XDG_CONFIG_HOME/gp/gpconfig.ini).
