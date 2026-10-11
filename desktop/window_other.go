//go:build !darwin && !windows && !linux

package main

import "fmt"

// Other systems get no native window yet: Zex opens in the default browser.
func runWindow(url string) error { return errNoWebView }

func fallback(url string, err error) {
	openURL(url)
	fmt.Println("Zex on", url, "(Ctrl+C to stop)")
	select {}
}
