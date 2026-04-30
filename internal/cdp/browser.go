package cdp

import (
	"context"
	"fmt"
	"runtime"

	"github.com/chromedp/chromedp"
)

type Mode int

const (
	ModeLaunch Mode = iota
	ModeAttach
)

type Options struct {
	Mode        Mode
	Headless    bool
	ChromePath  string // override exec path (launch mode)
	UserDataDir string // persistent profile dir (launch mode)
	RemoteURL   string // ws://host:port or http://host:port (attach mode)
}

type Browser struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc
	ctx         context.Context
	cancel      context.CancelFunc
	mode        Mode
}

// Default Chrome path on darwin if not overridden.
func defaultChromePath() string {
	if runtime.GOOS == "darwin" {
		return "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	}
	return ""
}

func New(parent context.Context, opts Options) (*Browser, error) {
	switch opts.Mode {
	case ModeLaunch:
		return newLaunched(parent, opts)
	case ModeAttach:
		return newAttached(parent, opts)
	default:
		return nil, fmt.Errorf("unknown mode")
	}
}

func newLaunched(parent context.Context, opts Options) (*Browser, error) {
	flags := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	flags = append(flags,
		chromedp.Flag("headless", opts.Headless),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if opts.UserDataDir != "" {
		flags = append(flags, chromedp.UserDataDir(opts.UserDataDir))
	}
	chromePath := opts.ChromePath
	if chromePath == "" {
		chromePath = defaultChromePath()
	}
	if chromePath != "" {
		flags = append(flags, chromedp.ExecPath(chromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, flags...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("launch chrome: %w", err)
	}
	return &Browser{allocCtx: allocCtx, allocCancel: allocCancel, ctx: ctx, cancel: cancel, mode: ModeLaunch}, nil
}

func newAttached(parent context.Context, opts Options) (*Browser, error) {
	if opts.RemoteURL == "" {
		return nil, fmt.Errorf("attach mode requires RemoteURL (e.g. http://localhost:9222)")
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(parent, opts.RemoteURL)
	ctx, cancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("attach chrome at %s: %w", opts.RemoteURL, err)
	}
	return &Browser{allocCtx: allocCtx, allocCancel: allocCancel, ctx: ctx, cancel: cancel, mode: ModeAttach}, nil
}

func (b *Browser) Ctx() context.Context { return b.ctx }

func (b *Browser) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.allocCancel != nil {
		b.allocCancel()
	}
}
