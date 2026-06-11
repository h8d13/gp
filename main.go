package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func urlScheme(url string, encrypt bool) string {
	if strings.Contains(url, "://") {
		return url
	}
	if encrypt {
		return "https://" + url
	}
	return "http://" + url
}

func main() {
	out := flag.String("o", "", "save response body to file")
	flag.Parse()

	p := loadPrefs("config.ini", ".env")

	url := "example.com"
	if flag.NArg() > 0 {
		url = flag.Arg(0)
	}
	url = urlScheme(url, p.AlwaysEncrypt)

	client := &http.Client{Timeout: 30 * time.Second}
	if p.AllowInsecure {
		client.Transport = &http.Transport{
			// custom TLSClientConfig disables automatic h2, re-enable
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		}
	}

	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*out, body, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "save:", err)
			os.Exit(1)
		}
	}

	fmt.Printf("%s %s proto=%s bytes=%d in %v\n", url, resp.Status, resp.Proto, len(body), time.Since(start))
}
