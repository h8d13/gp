// Command gp is a small HTTP downloader. All logic lives in the src
// package; this entry point just hands off to it.
package main

import "github.com/h8d13/gp/src"

func main() {
	src.Main()
}
