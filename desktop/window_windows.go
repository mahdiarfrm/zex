package main

import (
	"os"
	"path/filepath"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

func runWindow(url string) error {
	data := filepath.Join(os.Getenv("LOCALAPPDATA"), "Zex", "WebView2")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  data,
		WindowOptions: webview2.WindowOptions{
			Title:  "Zex",
			Width:  1360,
			Height: 860,
			IconId: 1, // the icon group compiled in by go-winres (desktop/winres)
			Center: true,
		},
	})
	if w == nil {
		return errNoWebView
	}
	defer w.Destroy()
	w.SetSize(900, 600, webview2.HintMin)
	w.Bind("zexOpen", openExternal)
	w.Bind("zexSave", saveDownload)
	w.Init(bridgeJS)
	w.Navigate(url)
	w.Run()
	return nil
}

// Without the WebView2 runtime (rare on Windows 10/11), run Zex in the browser and keep
// the server up until the user closes this message.
func fallback(url string, err error) {
	openURL(url)
	title, _ := windows.UTF16PtrFromString("Zex")
	text, _ := windows.UTF16PtrFromString("Zex is open in your browser because the Microsoft Edge WebView2 Runtime is not installed.\n\nKeep this message open while you use Zex. Close it to quit.\n\nTo get the Zex window, install WebView2 from go.microsoft.com/fwlink/p/?LinkId=2124703")
	windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONINFORMATION)
}
