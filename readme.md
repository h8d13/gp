# gp

`get-packets` is a `go` CLI that fetches over HTTP/1, HTTP/2, and HTTP/3.

> Inspired by some of [curl](https://github.com/curl/curl) but also doubles as a **sources and extract** manager. 

See [config](./config.ini) and [sources](./sources.ini) for examples.

---
<!-- gp-help:begin (generated; edit flags or gen-readme.sh, not below) -->

## Usage

```
Usage: gp [flags] URL
       gp up [flags]

Commands:
  gp up
    	install/update all tools from sources.ini

Global options (all commands):
  -p, --parallel int
    	parallel connections; 1 disables
  -q, --quic
    	use HTTP/3 over QUIC
  -z, --compress
    	accept gzip/zstd/br transfer encoding (disables ranges/resume/304)
  -r, --retries int
    	retries on 429/503; 0 disables
  -c, --chunk int
    	split/resume chunk size in bytes
  -n, --no-progress
    	disable the live download progress line

Download options (URL form only):
  -o, --output string
    	save the response body to this path (otherwise stdout)
  -x, --extract string
    	unpack the downloaded tar (gz/zst/xz/bz2) into this dir
  -f, --force
    	re-download even if the cached copy is still current
```

## Configuration

Keys resolve in order:

command-line flags > real `env` vars > `.env` in $CWD > `$XDG_CONFIG_HOME/gp/config.ini` > fallback defaults.
