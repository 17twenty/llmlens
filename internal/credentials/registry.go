package credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	icdp "llmlens/internal/cdp"
)

// Registry is a domain → Bundle map backed by a directory of *.json files.
//
//	Apply  imports cookies + storage for every loaded profile.
//	Watch  monitors the directory and applies new/updated profiles in-place.
//
// The watcher is the load-bearing UX win: when an agent hits an auth wall
// mid-conversation, the user can run `llmlens auth-start --domain=X` in
// another terminal; the running MCP server picks up the new profile and the
// agent's next snapshot will see auth_required clear.
type Registry struct {
	mu       sync.RWMutex
	dir      string
	profiles map[string]*Bundle // keyed by Bundle.Domain
	onLog    func(string)
}

func NewRegistry(dir string, onLog func(string)) *Registry {
	if onLog == nil {
		onLog = func(string) {}
	}
	return &Registry{dir: dir, profiles: map[string]*Bundle{}, onLog: onLog}
}

// LoadAll reads every *.json under the registry dir into memory. Per-file
// errors are logged but not fatal — one malformed bundle shouldn't block the
// rest from loading.
func (r *Registry) LoadAll() error {
	if _, err := os.Stat(r.dir); os.IsNotExist(err) {
		return nil // empty dir is fine; watcher will create-on-demand
	}
	matches, err := filepath.Glob(filepath.Join(r.dir, "*.json"))
	if err != nil {
		return fmt.Errorf("glob profiles dir: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range matches {
		b, err := LoadBundle(p)
		if err != nil {
			r.onLog(fmt.Sprintf("warn: skipping %s: %v", filepath.Base(p), err))
			continue
		}
		if b.Domain == "" {
			r.onLog(fmt.Sprintf("warn: %s has empty domain, skipping", filepath.Base(p)))
			continue
		}
		r.profiles[b.Domain] = b
	}
	return nil
}

// Apply imports every loaded profile into the browser. Calls Import per
// bundle — cookies merge naturally in the browser cookie jar (different
// domains don't conflict), and storage replay navigates to each origin in
// turn before the agent does any work.
func (r *Registry) Apply(ctx context.Context, b *icdp.Browser) error {
	r.mu.RLock()
	bundles := make([]*Bundle, 0, len(r.profiles))
	for _, bd := range r.profiles {
		bundles = append(bundles, bd)
	}
	r.mu.RUnlock()

	if len(bundles) == 0 {
		return nil
	}

	for _, bd := range bundles {
		if err := Import(ctx, b, bd); err != nil {
			return fmt.Errorf("import %s: %w", bd.Domain, err)
		}
	}

	domains := make([]string, 0, len(bundles))
	for _, bd := range bundles {
		domains = append(domains, bd.Domain)
	}
	r.onLog(fmt.Sprintf("imported %d profile(s): %s", len(bundles), strings.Join(domains, ", ")))
	return nil
}

// Watch installs a fsnotify watcher on the registry dir. New or modified
// *.json files are loaded and applied to the live browser on the spot.
// Returns immediately; the watcher runs in a goroutine until ctx is cancelled.
func (r *Registry) Watch(ctx context.Context, b *icdp.Browser) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		w.Close()
		return fmt.Errorf("ensure profiles dir: %w", err)
	}
	if err := w.Add(r.dir); err != nil {
		w.Close()
		return fmt.Errorf("watch %s: %w", r.dir, err)
	}

	go func() {
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !strings.HasSuffix(ev.Name, ".json") {
					continue
				}
				// Create + Write cover both "atomic write via rename" and
				// "in-place modification" patterns.
				if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
					continue
				}
				r.handleFileChange(ctx, b, ev.Name)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				r.onLog(fmt.Sprintf("watch error: %v", err))
			}
		}
	}()
	return nil
}

func (r *Registry) handleFileChange(ctx context.Context, b *icdp.Browser, path string) {
	bundle, err := LoadBundle(path)
	if err != nil {
		// Likely partial write — fsnotify can fire mid-flush. The follow-up
		// Write event will fire when the file is complete.
		r.onLog(fmt.Sprintf("hot-reload: skip %s (%v) — will retry on next event", filepath.Base(path), err))
		return
	}
	if bundle.Domain == "" {
		return
	}
	if err := Import(ctx, b, bundle); err != nil {
		r.onLog(fmt.Sprintf("hot-reload warn: applying %s: %v", bundle.Domain, err))
		return
	}
	r.mu.Lock()
	r.profiles[bundle.Domain] = bundle
	r.mu.Unlock()
	r.onLog(fmt.Sprintf("hot-reload: imported %s (%d cookies, %d origin(s) of storage)",
		bundle.Domain, len(bundle.Cookies), len(bundle.Origins)))
}
