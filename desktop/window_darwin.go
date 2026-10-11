package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// A plain app gets no menu bar, and without an Edit menu WebKit ignores Cmd+C, Cmd+V and
// friends. This adds the standard App, Edit and Window menus.
static void zexMenus(void) {
	NSMenu *bar = [NSMenu new];

	NSMenuItem *appItem = [NSMenuItem new];
	NSMenu *app = [NSMenu new];
	[app addItemWithTitle:@"About Zex" action:@selector(orderFrontStandardAboutPanel:) keyEquivalent:@""];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItemWithTitle:@"Hide Zex" action:@selector(hide:) keyEquivalent:@"h"];
	[app addItem:[NSMenuItem separatorItem]];
	[app addItemWithTitle:@"Quit Zex" action:@selector(terminate:) keyEquivalent:@"q"];
	[appItem setSubmenu:app];
	[bar addItem:appItem];

	NSMenuItem *editItem = [NSMenuItem new];
	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
	[edit addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
	[edit addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"Z"];
	[edit addItem:[NSMenuItem separatorItem]];
	[edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
	[edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
	[edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
	[edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
	[editItem setSubmenu:edit];
	[bar addItem:editItem];

	NSMenuItem *winItem = [NSMenuItem new];
	NSMenu *win = [[NSMenu alloc] initWithTitle:@"Window"];
	[win addItemWithTitle:@"Minimize" action:@selector(performMiniaturize:) keyEquivalent:@"m"];
	[win addItemWithTitle:@"Close" action:@selector(performClose:) keyEquivalent:@"w"];
	[winItem setSubmenu:win];
	[bar addItem:winItem];

	[NSApp setMainMenu:bar];
	[NSApp setWindowsMenu:win];
}

// Native look: the page runs under a transparent title bar, with the window buttons on top.
static void zexChrome(void *ptr) {
	NSWindow *w = (NSWindow *)ptr;
	w.styleMask |= NSWindowStyleMaskFullSizeContentView;
	w.titlebarAppearsTransparent = YES;
	w.titleVisibility = NSWindowTitleHidden;
}

// Called from a mouse-down in the page's top strip: hand the drag to the window server.
static void zexDrag(void *ptr) {
	NSEvent *e = [NSApp currentEvent];
	if (e) [(NSWindow *)ptr performWindowDragWithEvent:e];
}

// Double-click on the top strip does what the system setting says (zoom by default).
static void zexTitleDoubleClick(void *ptr) {
	NSWindow *w = (NSWindow *)ptr;
	NSString *action = [[NSUserDefaults standardUserDefaults] stringForKey:@"AppleActionOnDoubleClick"];
	if ([action isEqualToString:@"Minimize"]) [w performMiniaturize:nil];
	else if (![action isEqualToString:@"None"]) [w performZoom:nil];
}
*/
import "C"

import webview "github.com/webview/webview_go"

// macTitleBarJS marks the page as the macOS app (for its CSS) and makes the top strip of the
// window drag it, like a title bar, everywhere except on controls.
const macTitleBarJS = `(() => {
  const mark = () => document.documentElement && document.documentElement.setAttribute("data-desktop", "mac");
  mark(); document.addEventListener("DOMContentLoaded", mark);
  document.addEventListener("mousedown", (e) => {
    if (e.button !== 0 || e.clientY > 44) return;
    if (e.target.closest("button, a, input, select, textarea, label, summary, canvas, [role=tab], [contenteditable]")) return;
    if (e.detail === 2) window.zexTitleDoubleClick(); else window.zexDrag();
  });
})();`

func runWindow(url string) error {
	w := webview.New(false)
	if w == nil {
		return errNoWebView
	}
	defer w.Destroy()
	C.zexMenus()
	w.SetTitle("Zex")
	w.SetSize(960, 640, webview.HintMin)
	w.SetSize(1360, 860, webview.HintNone)
	win := w.Window()
	C.zexChrome(win)
	w.Bind("zexOpen", openExternal)
	w.Bind("zexSave", saveDownload)
	w.Bind("zexDrag", func() { C.zexDrag(win) })
	w.Bind("zexTitleDoubleClick", func() { C.zexTitleDoubleClick(win) })
	w.Init(bridgeJS + macTitleBarJS)
	w.Navigate(url)
	w.Run()
	return nil
}

// fallback should not happen on macOS (WebKit is part of the system); use the browser.
func fallback(url string, err error) {
	openURL(url)
	select {}
}
