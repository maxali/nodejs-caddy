package nodejs

import (
	"context"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

func init() {
	caddy.RegisterModule(Nodejs{})
	httpcaddyfile.RegisterHandlerDirective("nodejs", parseCaddyfile)
}

type Nodejs struct {
	File string

	serverMutex sync.Mutex
	lastActive  time.Time
	serverCmd   *exec.Cmd
	serverAddr  string
	timeout     time.Duration
}

func (Nodejs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nodejs",
		New: func() caddy.Module { return new(Nodejs) },
	}
}

func (n *Nodejs) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	n.serverMutex.Lock()
	defer n.serverMutex.Unlock()

	if n.serverCmd == nil || n.serverCmd.ProcessState != nil && n.serverCmd.ProcessState.Exited() {
		err := n.startServer()
		if err != nil {
			return fmt.Errorf("failed to start node.js server: %v", err)
		}
	}

	n.lastActive = time.Now()

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, n.serverAddr+r.URL.Path, r.Body)
	if err != nil {
		return err
	}
	proxyReq.Header = r.Header

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	for header, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)

	if time.Since(n.lastActive) > n.timeout {
		n.stopServer()
	}

	return nil
}

func (n *Nodejs) startServer() error {
	n.serverCmd = exec.Command("node", n.File)
	err := n.serverCmd.Start()
	if err != nil {
		return err
	}
	n.serverAddr = "http://localhost:3000"
	n.timeout = 1 * time.Minute

	go func() {
		<-time.After(n.timeout)
		n.serverMutex.Lock()
		defer n.serverMutex.Unlock()
		if time.Since(n.lastActive) > n.timeout {
			n.stopServer()
		}
	}()

	return nil
}

func (n *Nodejs) stopServer() {
	if n.serverCmd != nil && n.serverCmd.Process != nil {
		n.serverCmd.Process.Kill()
		n.serverCmd.Process.Release()
		n.serverCmd = nil
	}
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var n Nodejs
	for h.Next() {
		for h.NextBlock(0) {
			switch h.Val() {
			case "file":
				if !h.AllArgs(&n.File) {
					return nil, h.ArgErr()
				}
			default:
				return nil, h.Errf("unrecognized parameter '%s'", h.Val())
			}
		}
	}
	return n, nil
}