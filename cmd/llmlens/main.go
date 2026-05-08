package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	icdp "llmlens/internal/cdp"
	"llmlens/internal/credentials"
	"llmlens/internal/debug"
	"llmlens/internal/rpc"
	"llmlens/internal/session"
	"llmlens/internal/tools"
)

const usage = `llmlens — thin CDP-native harness for LLM browser agents

Subcommands:
  serve            run JSON-RPC server on stdio
  auth-start       open a browser, watch for login, auto-export the result
  export-creds     dump cookies + web storage from an already-running Chrome
  help             show this message

Run 'llmlens <subcommand> -h' for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "auth-start":
		cmdAuthStart(os.Args[2:])
	case "export-creds":
		cmdExportCreds(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	mode := fs.String("mode", "launch", "launch | attach")
	protocol := fs.String("protocol", "jsonrpc", "jsonrpc | mcp (use mcp for Claude Code / MCP-aware clients)")
	headless := fs.Bool("headless", false, "run launched chrome headless")
	chromePath := fs.String("chrome", "", "override chrome executable (launch mode)")
	userDataDir := fs.String("user-data-dir", "", "persistent profile dir (launch mode)")
	remoteURL := fs.String("remote", "http://localhost:9222", "CDP endpoint (attach mode)")
	profilesDir := fs.String("profiles-dir", "profiles", "directory of *.json credential bundles, hot-reloaded on change")
	runsDir := fs.String("runs-dir", "runs", "root dir for session artifacts")
	_ = fs.Parse(args)

	ctx, cancel := signalContext()
	defer cancel()

	// Open the debug log if LLMLENS_DEBUG_LOG is set. No-op otherwise.
	debug.Init()
	defer debug.Close()
	debug.Logf("serve", "start mode=%s protocol=%s profiles-dir=%s",
		*mode, *protocol, *profilesDir)

	opts := icdp.Options{
		Headless:    *headless,
		ChromePath:  *chromePath,
		UserDataDir: *userDataDir,
		RemoteURL:   *remoteURL,
	}
	switch *mode {
	case "launch":
		opts.Mode = icdp.ModeLaunch
	case "attach":
		opts.Mode = icdp.ModeAttach
	default:
		fmt.Fprintf(os.Stderr, "invalid mode %q\n", *mode)
		os.Exit(2)
	}

	// Eagerly load every bundle in profiles/ so config errors fail at startup
	// rather than the first tool call.
	registry := credentials.NewRegistry(*profilesDir, func(msg string) {
		fmt.Fprintln(os.Stderr, "  ", msg)
	})
	if err := registry.LoadAll(); err != nil {
		fmt.Fprintln(os.Stderr, "load profiles:", err)
		os.Exit(1)
	}

	sess, err := session.New(*runsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "session:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "llmlens serve: run-id=%s artifacts=%s profiles-dir=%s (browser launches on first tool call)\n",
		sess.RunID, sess.Root, *profilesDir)

	// Apply all profiles when the browser actually launches, then start the
	// hot-reload watcher. The watcher runs until ctx is cancelled.
	afterLaunch := func(b *icdp.Browser) error {
		if err := registry.Apply(ctx, b); err != nil {
			return err
		}
		if err := registry.Watch(ctx, b); err != nil {
			fmt.Fprintln(os.Stderr, "  warn: hot-reload disabled:", err)
		}
		return nil
	}

	engine := tools.New(ctx, opts, afterLaunch, sess)
	defer engine.Close()
	srv := rpc.New(engine, os.Stdin, os.Stdout)
	switch *protocol {
	case "jsonrpc":
		srv.WithMode(rpc.ModeJSONRPC)
	case "mcp":
		srv.WithMode(rpc.ModeMCP)
		fmt.Fprintln(os.Stderr, "llmlens serve: MCP transport active")
	default:
		fmt.Fprintf(os.Stderr, "invalid protocol %q (jsonrpc | mcp)\n", *protocol)
		os.Exit(2)
	}
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "rpc:", err)
		os.Exit(1)
	}
}

func cmdAuthStart(args []string) {
	fs := flag.NewFlagSet("auth-start", flag.ExitOnError)
	domain := fs.String("domain", "", "domain to log in to, e.g. linkedin.com")
	out := fs.String("out", "", "output JSON path for the credential bundle")
	settle := fs.Duration("settle", 3*time.Second, "URL must stay off /login* for this duration before capture")
	timeout := fs.Duration("timeout", 5*time.Minute, "give up if no login completes in this window")
	keepOpen := fs.Bool("keep-open", false, "leave the browser open after capture")
	_ = fs.Parse(args)

	if *domain == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "auth-start: -domain and -out are required")
		os.Exit(2)
	}

	ctx, cancel := signalContext()
	defer cancel()

	fmt.Fprintf(os.Stderr, "→ A Chrome window will open. Log in to %s; capture fires once you reach a stable, non-login URL.\n", *domain)
	bundle, err := credentials.Start(ctx, credentials.StartOptions{
		Domain:     *domain,
		SettleTime: *settle,
		Timeout:    *timeout,
		KeepOpen:   *keepOpen,
		OnLog: func(msg string) {
			fmt.Fprintf(os.Stderr, "  %s\n", msg)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth-start:", err)
		os.Exit(1)
	}
	if err := credentials.SaveBundle(bundle, *out); err != nil {
		fmt.Fprintln(os.Stderr, "save:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "✓ Saved %s — %d cookies, %d origin(s) of storage\n",
		*out, len(bundle.Cookies), len(bundle.Origins))
	if *keepOpen {
		fmt.Fprintln(os.Stderr, "  browser left open (-keep-open). Ctrl+C to exit.")
		<-ctx.Done()
	}
}

func cmdExportCreds(args []string) {
	fs := flag.NewFlagSet("export-creds", flag.ExitOnError)
	remoteURL := fs.String("remote", "http://localhost:9222", "CDP endpoint of running browser")
	domain := fs.String("domain", "", "domain to export, e.g. linkedin.com")
	out := fs.String("out", "", "output JSON path")
	_ = fs.Parse(args)

	if *domain == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "export-creds: -domain and -out are required")
		os.Exit(2)
	}

	ctx, cancel := signalContext()
	defer cancel()

	bundle, err := credentials.Export(ctx, *remoteURL, *domain)
	if err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
	if err := credentials.SaveBundle(bundle, *out); err != nil {
		fmt.Fprintln(os.Stderr, "save:", err)
		os.Exit(1)
	}
	storageOrigins := 0
	for range bundle.Origins {
		storageOrigins++
	}
	fmt.Fprintf(os.Stderr, "exported %d cookies + %d origin(s) of storage to %s\n",
		len(bundle.Cookies), storageOrigins, *out)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
