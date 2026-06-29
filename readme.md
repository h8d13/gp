# gp

`get-packets` is a small `go` CLI that fetches over HTTP/1, HTTP/2, and HTTP/3.

---
<!-- gp-help:begin (generated; edit flags or gen-readme.sh, not below) -->

## Usage

```
Usage: gp [flags] [url]   (url defaults to example.com)

  -o, --output string
    	save response body to file
  -p, --parallel int
    	parallel connections; 1 disables (needs -o)
  -q, --quic
    	use HTTP/3 over QUIC
  -r, --retries int
    	retries on 429/503; 0 disables
  -c, --chunk int
    	split/resume chunk size in bytes
```

## Configuration

Keys resolve in order:

command-line flags > real `env` vars > `.env` in $CWD > `$XDG_CONFIG_HOME/gp/gpconfig.ini` > fallback defaults.
