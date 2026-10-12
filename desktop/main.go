// Command zex is the Zex desktop app: the web UI and its relay in one binary, shown in a
// native window (WebKit on macOS, WebView2 on Windows, WebKitGTK on Linux).
package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"zex/web"
)

var version = "0.4.0"

var errNoWebView = errors.New("no web view available")

// Cocoa wants the window on the main thread.
func init() { runtime.LockOSThread() }

func main() {
	port := flag.Int("port", 18765, "local port for the UI and relay")
	browser := flag.Bool("browser", false, "open in the default browser instead of a window")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println("zex", version)
		return
	}

	ln, p, existing := listen(*port)
	url := existing
	if ln != nil {
		url = fmt.Sprintf("http://127.0.0.1:%d/", p)
		go http.Serve(ln, newHandler(web.FS, p))
	}
	url += "#/home"

	if *browser {
		openURL(url)
		fmt.Println("Zex on", url, "(Ctrl+C to stop)")
		select {}
	}
	if err := runWindow(url); err != nil {
		fallback(url, err)
	}
}

// bridgeJS runs before the page on every load. App web views don't open new tabs or save
// downloads, so external links go to the default browser and CSV exports to Downloads.
const bridgeJS = `(() => {
  const ext = (href) => { try { const u = new URL(href, location.href); return /^https?:$/.test(u.protocol) && u.origin !== location.origin ? u.href : null; } catch { return null; } };
  document.addEventListener("click", (e) => {
    const a = e.target.closest && e.target.closest("a[href]");
    const u = a && !a.hasAttribute("download") && ext(a.getAttribute("href"));
    if (u) { e.preventDefault(); window.zexOpen(u); }
  }, true);
  const open = window.open;
  window.open = (u, ...rest) => { const x = ext(u); if (x) { window.zexOpen(x); return null; } return open.call(window, u, ...rest); };
  const click = HTMLAnchorElement.prototype.click;
  HTMLAnchorElement.prototype.click = function () {
    if (this.hasAttribute("download") && /^(blob|data):/.test(this.href)) {
      fetch(this.href).then((r) => r.text()).then((t) => window.zexSave(this.getAttribute("download") || "zex-export.txt", t));
      return;
    }
    return click.call(this);
  };
  window.zexDesktop = true;
})();`

// openExternal is bound as window.zexOpen: web links only.
func openExternal(u string) error {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("only web links open outside Zex")
	}
	return openURL(u)
}

var unsafeName = regexp.MustCompile(`[^\w.\- ]+`)

// saveDownload is bound as window.zexSave: writes an export to Downloads and shows it.
func saveDownload(name, content string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name = unsafeName.ReplaceAllString(filepath.Base(name), "_")
	if name == "" || name == "." {
		name = "zex-export.txt"
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	path := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	revealFile(path)
	return path, nil
}

func openURL(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

// readPicked hands a file chosen in a native open panel to the page, as base64.
func readPicked(path string) (map[string]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > 50<<20 {
		return nil, errors.New("that file is over 50 MB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"name": filepath.Base(path), "data": base64.StdEncoding.EncodeToString(data)}, nil
}

// revealFile shows a saved file in Finder or Explorer; tests swap it out.
var revealFile = reveal

func reveal(path string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", "-R", path).Start()
	case "windows":
		exec.Command("explorer", "/select,", path).Start()
	default:
		exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}
