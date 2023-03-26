package nodejs

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LogWriter struct {
	logger *zap.Logger
	level  zapcore.Level
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.logger.Check(w.level, msg).Write()
	}
	return len(p), nil
}

func init() {
	caddy.RegisterModule(&Nodejs{})
	httpcaddyfile.RegisterHandlerDirective("nodejs", parseCaddyfile)
}

type Nodejs struct {
	File string
	Port int

	serverMutex sync.Mutex
	lastActive  time.Time
	serverCmd   *exec.Cmd
	serverAddr  string
	timeout     time.Duration

	logger *zap.Logger
}

func (n *Nodejs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nodejs",
		New: func() caddy.Module { return new(Nodejs) },
	}
}

func (n *Nodejs) Provision(ctx caddy.Context) error {
	n.logger = ctx.Logger(n)
	return nil
}

func (n *Nodejs) startServer() error {
	cmd := exec.Command("node", n.File)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", n.Port))
	n.logger.Debug(strings.Join(cmd.Env, ","))

	// Redirect stdout and stderr to the logger
	cmd.Stdout = &LogWriter{logger: n.logger, level: zap.InfoLevel}
	cmd.Stderr = &LogWriter{logger: n.logger, level: zap.ErrorLevel}

	if err := cmd.Start(); err != nil {
		return err
	}

	n.serverCmd = cmd
	n.serverAddr = fmt.Sprintf("http://localhost:%d", n.Port)
	n.timeout = 1 * time.Minute

	n.logger.Debug(fmt.Sprintf("Server started on process %d", cmd.Process.Pid))

	return nil
}

func (n *Nodejs) stopServer(pid int) {
	n.logger.Debug("Stopping server", zap.Int("pid", pid))
	n.serverMutex.Lock()
	defer n.serverMutex.Unlock()

	// Send a SIGTERM signal to the specified process ID
	process, err := os.FindProcess(pid)
	if err == nil {
		process.Signal(os.Interrupt)
	}

	// Wait for the serverCmd process to exit
	if n.serverCmd != nil && n.serverCmd.Process != nil && n.serverCmd.Process.Pid == pid {
		n.serverCmd.Process.Wait()
	}

	n.logger.Debug("Server stopped", zap.Int("pid", pid))
}

func (n *Nodejs) monitorIdleTime(pid int) {
	for {
		time.Sleep(60 * time.Second)
		n.logger.Debug("Checking if server is idle", zap.Int("pid", pid))
		n.serverMutex.Lock()
		if time.Since(n.lastActive) > n.timeout {
			n.stopServer(pid)
			n.serverMutex.Unlock()
			break
		}
		n.serverMutex.Unlock()
	}
}

func (n *Nodejs) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	n.logger.Debug("Handling request")

	n.serverMutex.Lock()
	defer n.serverMutex.Unlock()

	if n.serverCmd == nil || n.serverCmd.ProcessState != nil && n.serverCmd.ProcessState.Exited() {
		n.logger.Debug("Starting new server")
		err := n.startServer()
		if err != nil {
			return fmt.Errorf("failed to start node.js server: %v", err)
		}
		go n.monitorIdleTime(n.serverCmd.Process.Pid)
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

	return nil
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
			case "port":
				if !h.NextArg() {
					return nil, h.ArgErr()
				}
				port, err := strconv.Atoi(h.Val())
				if err != nil {
					return nil, h.Errf("invalid port: %v", err)
				}
				n.Port = port
			default:
				return nil, h.Errf("unrecognized parameter '%s'", h.Val())
			}
		}
	}

	if n.Port == 0 {
		n.Port = 3000
	}
	return &n, nil
}
