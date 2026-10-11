package main

import webview "github.com/webview/webview_go"

// Linux uses WebKitGTK. The window keeps its GTK title bar, which matches the desktop theme.
func runWindow(url string) error {
	w := webview.New(false)
	if w == nil {
		return errNoWebView
	}
	defer w.Destroy()
	w.SetTitle("Zex")
	w.SetSize(960, 640, webview.HintMin)
	w.SetSize(1360, 860, webview.HintNone)
	w.Bind("zexOpen", openExternal)
	w.Bind("zexSave", saveDownload)
	w.Init(bridgeJS)
	w.Navigate(url)
	w.Run()
	return nil
}

func fallback(url string, err error) {
	openURL(url)
	select {}
}
