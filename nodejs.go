package nodejs

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	docker "github.com/fsouza/go-dockerclient"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LogWriter struct {
	logger *zap.Logger
	level  zapcore.Level
}

func (lw *LogWriter) Write(p []byte) (n int, err error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		lw.logger.Check(lw.level, message).Write()
	}
	return len(p), nil
}

func init() {
	caddy.RegisterModule(&Nodejs{})
	httpcaddyfile.RegisterHandlerDirective("nodejs", parseCaddyfile)
}

type Nodejs struct {
	Port        int
	serverMutex sync.Mutex
	lastActive  time.Time
	serverAddr  string
	timeout     time.Duration
	logger      *zap.Logger

	App         string
	Entrypoint  string
	Command     string
	containerID string
}

func (n *Nodejs) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.nodejs",
		New: func() caddy.Module { return new(Nodejs) },
	}
}

func (n *Nodejs) Provision(ctx caddy.Context) error {
	config := zap.NewDevelopmentConfig()
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := config.Build()
	if err != nil {
		return err
	}
	n.logger = logger.Named("nodejs")
	return nil
}

func newDockerClient() (*docker.Client, error) {
	client, err := docker.NewClientFromEnv()
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (n *Nodejs) startServer(lockAcquired bool) error {
	if !lockAcquired {
		n.serverMutex.Lock()
		defer n.serverMutex.Unlock()
	}

	// Create a new Docker client
	client, err := newDockerClient()
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %v", err)
	}

	// Set up the container configuration
	config := &docker.Config{
		Image:        "maxali/node:14",
		Entrypoint:   []string{n.Entrypoint},
		Cmd:          []string{n.Command},
		ExposedPorts: map[docker.Port]struct{}{docker.Port(fmt.Sprintf("%d/tcp", n.Port)): {}},
		Env: []string{
			fmt.Sprintf("ENTRY_COMMAND=%s", n.Entrypoint),
			fmt.Sprintf("APP_ENTRY_FILE=%s", n.Command),
			fmt.Sprintf("PORT=%d", n.Port),
		},
	}

	hostConfig := &docker.HostConfig{
		Binds:        []string{fmt.Sprintf("%s:/app", n.App)},
		PortBindings: map[docker.Port][]docker.PortBinding{docker.Port(fmt.Sprintf("%d/tcp", n.Port)): {{HostIP: "0.0.0.0", HostPort: fmt.Sprintf("%d", n.Port)}}},
	}

	// Create and start the container
	container, err := client.CreateContainer(docker.CreateContainerOptions{
		Config:     config,
		HostConfig: hostConfig,
	})
	if err != nil {
		return fmt.Errorf("failed to create Docker container: %v", err)
	}

	err = client.StartContainer(container.ID, hostConfig)
	if err != nil {
		return fmt.Errorf("failed to start Docker container: %v", err)
	}

	n.logger.Debug("Container started with Id " + container.ID)
	n.containerID = container.ID

	// Set the server address
	n.serverAddr = fmt.Sprintf("http://localhost:%d", n.Port)

	go n.monitorIdleTime()
	n.logger.Debug("Server started.")

	return nil
}

func (n *Nodejs) stopServer(lockAcquired bool) {
	n.logger.Debug("Stopping server")

	if !lockAcquired {
		n.serverMutex.Lock()
		defer n.serverMutex.Unlock()
	}

	client, err := newDockerClient()
	if err != nil {
		n.logger.Error("Failed to create Docker client", zap.Error(err))
		return
	}

	err = client.StopContainer(n.containerID, 5) // 5 seconds timeout
	if err != nil {
		n.logger.Error("Failed to stop Docker container", zap.Error(err))
		return
	}

	err = client.RemoveContainer(docker.RemoveContainerOptions{
		ID:    n.containerID,
		Force: true,
	})
	if err != nil {
		n.logger.Error("Failed to remove Docker container", zap.Error(err))
		return
	}

	n.containerID = ""

	n.logger.Debug("Server stopped")
}

func (n *Nodejs) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	n.logger.Debug("Handling request")
	n.serverMutex.Lock()

	if n.containerID == "" {
		n.logger.Debug("Starting new server")
		err := n.startServer(true)
		if err != nil {
			return fmt.Errorf("failed to start node.js server: %v", err)
		}
	} else {
		n.logger.Debug("Server is already up.")
	}

	// Update the lastActive field every time a request is handled
	n.lastActive = time.Now()

	n.serverMutex.Unlock()

	n.logger.Debug("Starting new request: " + n.serverAddr + r.URL.Path)
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, n.serverAddr+r.URL.Path, r.Body)
	if err != nil {
		return err
	}
	proxyReq.Header = r.Header
	var httpClient = &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := httpClient.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("failed to proxy request: %v", err)
	}
	defer resp.Body.Close()

	// Add this block of logging
	n.logger.Debug("Received response from Node.js server",
		zap.Int("status", resp.StatusCode),
		zap.Any("headers", resp.Header),
	)

	for header, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		n.logger.Error("Failed to copy response", zap.Error(err))
		return fmt.Errorf("failed to copy response: %v", err)
	}

	return nil
}

func (n *Nodejs) monitorIdleTime() {
	for {
		time.Sleep(60 * time.Second)
		n.logger.Debug("Checking if server is idle")
		n.serverMutex.Lock()
		if time.Since(n.lastActive) > n.timeout {
			n.stopServer(true)
			n.serverMutex.Unlock()
			break
		}
		n.serverMutex.Unlock()
	}
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var n Nodejs
	for h.Next() {
		for h.NextBlock(0) {
			switch h.Val() {
			case "port":
				if !h.NextArg() {
					return nil, h.ArgErr()
				}
				port, err := strconv.Atoi(h.Val())
				if err != nil {
					return nil, h.Errf("invalid port: %v", err)
				}
				n.Port = port
			case "app":
				if !h.AllArgs(&n.App) {
					return nil, h.ArgErr()
				}
			case "entrypoint":
				if !h.AllArgs(&n.Entrypoint) {
					return nil, h.ArgErr()
				}
			case "command":
				if !h.AllArgs(&n.Command) {
					return nil, h.ArgErr()
				}
			default:
				return nil, h.Errf("unrecognized parameter '%s'", h.Val())
			}
		}
	}

	// If the port is not specified, assign a random port in the range 9000-9999
	if n.Port == 0 {
		port, err := getRandomPort(9000, 9999)
		if err != nil {
			return nil, h.Errf("could not find an available port: %v", err)
		}
		n.Port = port
	}

	return &n, nil
}

func getRandomPort(min, max int) (int, error) {
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < 100; i++ {
		port := rand.Intn(max-min+1) + min
		address := fmt.Sprintf(":%d", port)
		listener, err := net.Listen("tcp", address)
		if err == nil {
			listener.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not find an available port in the range %d-%d", min, max)
}
