//go:build windows
// +build windows

package webview2

import (
	"encoding/json"
	"log"
	"sync"
	"unsafe"

	"github.com/CanPacis/go-webview2/internal/w32"
	"github.com/CanPacis/go-webview2/pkg/edge"

	"golang.org/x/sys/windows"
)

var (
	windowContext     = map[uintptr]interface{}{}
	windowContextSync sync.RWMutex
)

func getWindowContext(wnd uintptr) interface{} {
	windowContextSync.RLock()
	defer windowContextSync.RUnlock()
	return windowContext[wnd]
}

func setWindowContext(wnd uintptr, data interface{}) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	windowContext[wnd] = data
}

type browser interface {
	Embed(hwnd uintptr) bool
	Resize()
	Navigate(url string)
	Init(script string)
	Eval(script string)
	NotifyParentWindowPositionChanged() error
	Focus()
}

type webview struct {
	hwnd       uintptr
	mainthread uintptr
	browser    browser
	autofocus  bool
	maxsz      w32.Point
	minsz      w32.Point
	m          sync.Mutex
	wv2api     func(string) interface{}
	dispatchq  []func()
}

type WindowOptions struct {
	Title string
}

type WebViewOptions struct {
	Window unsafe.Pointer
	Debug  bool

	// WebView2API provides a WebView2API class to JavaScript client when set
	// to a handler function.
	WebView2API func(string) interface{}

	// DataPath specifies the datapath for the WebView2 runtime to use for the
	// browser instance.
	DataPath string

	// AutoFocus will try to keep the WebView2 widget focused when the window
	// is focused.
	AutoFocus bool

	// WindowOptions customizes the window that is created to embed the
	// WebView2 widget.
	WindowOptions WindowOptions
}

// New creates a new webview in a new window.
func New(debug bool) WebView { return NewWithOptions(WebViewOptions{Debug: debug}) }

// NewWindow creates a new webview using an existing window.
//
// Deprecated: Use NewWithOptions.
func NewWindow(debug bool, window unsafe.Pointer) WebView {
	return NewWithOptions(WebViewOptions{Debug: debug, Window: window})
}

// NewWithOptions creates a new webview using the provided options.
func NewWithOptions(options WebViewOptions) WebView {
	w := &webview{}
	w.autofocus = options.AutoFocus

	chromium := edge.NewChromium()
	chromium.MessageCallback = w.msgcb
	chromium.Debug = options.Debug
	chromium.DataPath = options.DataPath
	chromium.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)

	w.browser = chromium
	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()
	if !w.CreateWithOptions(options.WindowOptions) {
		return nil
	}

	if options.WebView2API != nil {
		w.initWebView2Api()
		w.wv2api = options.WebView2API
	}

	return w
}

type rpcMessage struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type rpcResponse struct {
	ID      int         `json:"id"`
	Payload interface{} `json:"payload"`
}

func jsString(v interface{}) string { b, _ := json.Marshal(v); return string(b) }

func (w *webview) msgcb(msg string) {
	d := rpcMessage{}
	if err := json.Unmarshal([]byte(msg), &d); err != nil {
		log.Printf("invalid RPC message: %v", err)
		return
	}

	if d.Method == "__webview2_api__" {
		result := w.wv2api(string(d.Params[0]))
		response := rpcResponse{
			ID:      d.ID,
			Payload: result,
		}

		encoded, err := json.Marshal(response)
		if err != nil {
			log.Printf("invalid RPC response: %v", err)
			return
		}

		err = w.PostMessage(string(encoded))

		if err != nil {
			log.Printf("could not encode message: %v", err)
			return
		}
	} else {
		log.Print("unknown opcode")
	}
}

func (w *webview) initWebView2Api() {
	script := `class WebView2API extends EventTarget {
  #handlers = {};
  #id = 0;
  constructor() {
    super();

    window.addEventListener("message", (event) => {
      const data = JSON.parse(event.data)
      const handler = this.#handlers[data.id];
      this.dispatchEvent(new CustomEvent("message", { detail: event }));

      if (handler) {
		handler.resolve(data.payload);
      }

      delete this.#handlers[data.id];
    });
  }

  async send(payload) {
    return new Promise((resolve, reject) => {
      if ("chrome" in window && "webview" in window.chrome) {
        window.chrome.webview.postMessage(
          JSON.stringify({ id: this.#id, method: "__webview2_api__", params: [payload] })
        );
        this.#handlers[this.#id] = { resolve, reject };
        this.#id++;
      } else {
        console.error("There is no webview context");
      }
    });
  }
}

window.WebView2API = WebView2API;
`
	w.Init(script)
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	if w, ok := getWindowContext(hwnd).(*webview); ok {
		switch msg {
		case w32.WMMove, w32.WMMoving:
			_ = w.browser.NotifyParentWindowPositionChanged()
		case w32.WMNCLButtonDown:
			_, _, _ = w32.User32SetFocus.Call(w.hwnd)
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		case w32.WMSize:
			w.browser.Resize()
		case w32.WMActivate:
			if wp == w32.WAInactive {
				break
			}
			if w.autofocus {
				w.browser.Focus()
			}
		case w32.WMClose:
			_, _, _ = w32.User32DestroyWindow.Call(hwnd)
		case w32.WMDestroy:
			w.Terminate()
		case w32.WMGetMinMaxInfo:
			lpmmi := (*w32.MinMaxInfo)(unsafe.Pointer(lp))
			if w.maxsz.X > 0 && w.maxsz.Y > 0 {
				lpmmi.PtMaxSize = w.maxsz
				lpmmi.PtMaxTrackSize = w.maxsz
			}
			if w.minsz.X > 0 && w.minsz.Y > 0 {
				lpmmi.PtMinTrackSize = w.minsz
			}
		default:
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		}
		return 0
	}
	r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func (w *webview) Create(debug bool, window unsafe.Pointer) bool {
	// This function signature stopped making sense a long time ago.
	// It is but legacy cruft at this point.
	return w.CreateWithOptions(WindowOptions{})
}

func (w *webview) CreateWithOptions(opts WindowOptions) bool {
	var hinstance windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &hinstance)

	icow, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCxIcon)
	icoh, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCyIcon)

	icon, _, _ := w32.User32LoadImageW.Call(uintptr(hinstance), 32512, icow, icoh, 0)

	className, _ := windows.UTF16PtrFromString("webview")
	wc := w32.WndClassExW{
		CbSize:        uint32(unsafe.Sizeof(w32.WndClassExW{})),
		HInstance:     hinstance,
		LpszClassName: className,
		HIcon:         windows.Handle(icon),
		HIconSm:       windows.Handle(icon),
		LpfnWndProc:   windows.NewCallback(wndproc),
	}
	_, _, _ = w32.User32RegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	windowName, _ := windows.UTF16PtrFromString(opts.Title)
	w.hwnd, _, _ = w32.User32CreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0xCF0000,   // WS_OVERLAPPEDWINDOW
		0x80000000, // CW_USEDEFAULT
		0x80000000, // CW_USEDEFAULT
		640,
		480,
		0,
		0,
		uintptr(hinstance),
		0,
	)
	setWindowContext(w.hwnd, w)

	_, _, _ = w32.User32ShowWindow.Call(w.hwnd, w32.SWShow)
	_, _, _ = w32.User32UpdateWindow.Call(w.hwnd)
	_, _, _ = w32.User32SetFocus.Call(w.hwnd)

	if !w.browser.Embed(w.hwnd) {
		return false
	}
	w.browser.Resize()
	return true
}

func (w *webview) Destroy() {
}

func (w *webview) Run() {
	var msg w32.Msg
	for {
		_, _, _ = w32.User32GetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)
		if msg.Message == w32.WMApp {
			w.m.Lock()
			q := append([]func(){}, w.dispatchq...)
			w.dispatchq = []func(){}
			w.m.Unlock()
			for _, v := range q {
				v()
			}
		} else if msg.Message == w32.WMQuit {
			return
		}
		r, _, _ := w32.User32GetAncestor.Call(uintptr(msg.Hwnd), w32.GARoot)
		r, _, _ = w32.User32IsDialogMessage.Call(r, uintptr(unsafe.Pointer(&msg)))
		if r != 0 {
			continue
		}
		_, _, _ = w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *webview) Terminate() {
	_, _, _ = w32.User32PostQuitMessage.Call(0)
}

func (w *webview) Window() unsafe.Pointer {
	return unsafe.Pointer(w.hwnd)
}

func (w *webview) Navigate(url string) {
	w.browser.Navigate(url)
}

func (w *webview) SetTitle(title string) {
	_title, err := windows.UTF16FromString(title)
	if err != nil {
		_title, _ = windows.UTF16FromString("")
	}
	_, _, _ = w32.User32SetWindowTextW.Call(w.hwnd, uintptr(unsafe.Pointer(&_title[0])))
}

func (w *webview) SetSize(width int, height int, hints Hint) {
	index := w32.GWLStyle
	style, _, _ := w32.User32GetWindowLongPtrW.Call(w.hwnd, uintptr(index))
	if hints == HintFixed {
		style &^= (w32.WSThickFrame | w32.WSMaximizeBox)
	} else {
		style |= (w32.WSThickFrame | w32.WSMaximizeBox)
	}
	_, _, _ = w32.User32SetWindowLongPtrW.Call(w.hwnd, uintptr(index), style)

	if hints == HintMax {
		w.maxsz.X = int32(width)
		w.maxsz.Y = int32(height)
	} else if hints == HintMin {
		w.minsz.X = int32(width)
		w.minsz.Y = int32(height)
	} else {
		r := w32.Rect{}
		r.Left = 0
		r.Top = 0
		r.Right = int32(width)
		r.Bottom = int32(height)
		_, _, _ = w32.User32AdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), w32.WSOverlappedWindow, 0)
		_, _, _ = w32.User32SetWindowPos.Call(
			w.hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
			w32.SWPNoZOrder|w32.SWPNoActivate|w32.SWPNoMove|w32.SWPFrameChanged)
		w.browser.Resize()
	}
}

func (w *webview) Init(js string) {
	w.browser.Init(js)
}

func (w *webview) Eval(js string) {
	w.browser.Eval(js)
}

func (w *webview) Dispatch(f func()) {
	w.m.Lock()
	w.dispatchq = append(w.dispatchq, f)
	w.m.Unlock()
	_, _, _ = w32.User32PostThreadMessageW.Call(w.mainthread, w32.WMApp, 0, 0)
}

func (w *webview) PostMessage(message string) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}

	w.Dispatch(func() {
		w.Eval("window.postMessage(" + string(encoded) + ")")
	})

	return nil
}
