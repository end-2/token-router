package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"

	"go.yaml.in/yaml/v3"

	"token-router/src/internal/config"
	"token-router/src/internal/proxy"
)

type proxyGeneration struct {
	handler *proxy.Handler
	active  atomic.Int64
	retired atomic.Bool
}

func (p *proxyGeneration) retire() {
	p.retired.Store(true)
	p.handler.CloseIdleConnections()
}

func (p *proxyGeneration) release() {
	if p.active.Add(-1) == 0 && p.retired.Load() {
		// Active responses may return connections to the retired pool after a swap.
		p.handler.CloseIdleConnections()
	}
}

func (a *application) snapshot() config.PolicyConfig {
	a.policyMu.RLock()
	defer a.policyMu.RUnlock()
	return a.current.Clone()
}

// updatePolicy is serialized with file reads and writes by updateMu. All
// fallible preparation finishes before publishing the new request settings.
func (a *application) updatePolicy(next config.PolicyConfig, persist bool) error {
	a.policyMu.RLock()
	closed := a.policyClosed
	a.policyMu.RUnlock()
	if closed {
		return errPolicyClosed
	}
	if err := next.Apply(a.cfg).Validate(); err != nil {
		return err
	}
	current := a.snapshot()
	changed := !reflect.DeepEqual(next, current)
	proxyChanged := !reflect.DeepEqual(next.Upstream, current.Upstream) ||
		(next.RateLimit.TokensPerSecond > 0) != (current.RateLimit.TokensPerSecond > 0)
	var replacement *proxy.Handler
	if proxyChanged {
		var err error
		replacement, err = a.newProxyFor(next)
		if err != nil {
			return err
		}
	}
	if persist && a.cfg.Policy.File != "" {
		data, err := yaml.Marshal(next)
		if err == nil && len(data) > config.MaxPolicyBytes {
			err = errors.New("policy configuration exceeds size limit")
		}
		if err == nil {
			err = writePolicyFile(a.cfg.Policy.File, data)
		}
		if err != nil {
			if replacement != nil {
				replacement.CloseIdleConnections()
			}
			return errPersistPolicy
		}
		a.fileDigest, a.fileSeen = sha256.Sum256(data), true
	}
	if !changed {
		return nil
	}
	a.policyMu.Lock()
	if a.policyClosed {
		a.policyMu.Unlock()
		if replacement != nil {
			replacement.CloseIdleConnections()
		}
		return errPolicyClosed
	}
	a.limiter.Update(next.RateLimit)
	previous := a.proxy
	if replacement != nil {
		a.proxy = &proxyGeneration{handler: replacement}
	}
	a.current = next.Clone()
	a.policyMu.Unlock()
	if replacement != nil {
		previous.retire()
	}
	a.logger.Info("policy configuration updated")
	return nil
}

var errPersistPolicy = errors.New("cannot persist policy configuration file")
var errPolicyClosed = errors.New("server is shutting down")

func writePolicyFile(path string, data []byte) (err error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errPersistPolicy
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".policy-*.yaml")
	if err != nil {
		return errPersistPolicy
	}
	defer func() {
		if removeErr := os.Remove(f.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, errPersistPolicy)
		}
	}()
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errPersistPolicy
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return errPersistPolicy
	}
	return nil
}

func (a *application) reloadPolicy() error {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	data, err := config.ReadPolicyFile(a.cfg.Policy.File)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if a.fileSeen && a.fileDigest == digest {
		return nil
	}
	next, err := config.DecodePolicy(data, a.cfg.PolicyDefaults())
	if err == nil {
		err = a.updatePolicy(next, false)
	}
	if err != nil {
		return err
	}
	a.fileDigest, a.fileSeen = digest, true
	return nil
}

func (a *application) watchPolicy(ctx context.Context) {
	if a.cfg.Policy.File == "" {
		return
	}
	ticker := time.NewTicker(a.cfg.Policy.WatchInterval)
	defer ticker.Stop()
	failed := false
	for {
		if ctx.Err() != nil {
			return
		}
		if err := a.reloadPolicy(); err != nil {
			if !failed {
				a.logger.Warn("policy configuration reload failed; retaining active configuration", "error", err)
			}
			failed = true
		} else {
			failed = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *application) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		a.writeAdminJSON(w, http.StatusOK, a.snapshot())
	})
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		mux.HandleFunc(method+" /config", a.editPolicy)
	}
	return mux
}

func (a *application) editPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		a.writeAdminJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "server is not ready"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxPolicyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		status := http.StatusBadRequest
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			status = http.StatusRequestEntityTooLarge
		}
		a.writeAdminJSON(w, status, map[string]string{"error": "cannot read configuration request"})
		return
	}
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	base := a.cfg.PolicyDefaults()
	if r.Method == http.MethodPatch {
		base = a.snapshot()
	}
	next, err := config.DecodePolicy(data, base)
	if err == nil {
		err = a.updatePolicy(next, true)
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errPersistPolicy) {
			status = http.StatusInternalServerError
		} else if errors.Is(err, errPolicyClosed) {
			status = http.StatusServiceUnavailable
		}
		a.writeAdminJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	a.writeAdminJSON(w, http.StatusOK, a.snapshot())
}

func (a *application) writeAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		a.logger.Debug("admin response could not be written")
	}
}
