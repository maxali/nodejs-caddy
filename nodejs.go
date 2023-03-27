package nodejs

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

type TimeStampedWriter struct {
	underlying io.Writer
}

func (tsw *TimeStampedWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		formattedMsg := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
		return tsw.underlying.Write([]byte(formattedMsg))
	}
	return len(p), nil
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
	File                string
	Port                int
	serverStopped       bool
	serverReady         sync.WaitGroup
	serverMutex         sync.Mutex
	lastActive          time.Time
	serverCmd           *exec.Cmd
	serverAddr          string
	timeout             time.Duration
	logger              *zap.Logger
	LogFileMap          map[int]*os.File
	LogRotationDuration time.Duration
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

func (n *Nodejs) startServer() error {
	n.logger.Debug("Starting server")

	// Initialize the serverCmd field
	cmd := exec.Command("node", n.File)
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", n.Port))

	// Set the serverCmd field
	n.serverCmd = cmd
	n.serverStopped = false

	// Initialize the serverReady WaitGroup
	n.serverReady.Add(1)

	// Start the server
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start server: %v", err)
	}
	n.logger.Debug("After cmd.Start()")

	// Wait for the server to be ready in a separate goroutine
	go func() {
		n.logger.Debug("Waiting for starting server")

		for {
			conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", n.Port))

			if err == nil {
				n.logger.Debug("Connection: " + conn.LocalAddr().String())

				conn.Close()
				n.logger.Debug("Server is ready") // Add this line to log when the server is ready
				n.serverReady.Done()
				break
			}
			n.logger.Debug("Server not ready yet, retrying...") // Add this line to log when the server is not ready
			time.Sleep(100 * time.Millisecond)
		}
	}()
	// Set the server address
	n.serverAddr = fmt.Sprintf("http://localhost:%d", n.Port)

	n.logger.Debug(fmt.Sprintf("Started server with Pid: %d", n.serverCmd.Process.Pid))
	// Start monitoring the idle time of the server
	go n.monitorIdleTime(n.serverCmd.Process.Pid)

	// Set the log rotation duration; you can customize this value as needed
	n.LogRotationDuration = 1 * time.Hour

	// Initialize the log file map
	n.LogFileMap = make(map[int]*os.File)

	if err := n.createLogFile(n.serverCmd.Process.Pid); err != nil {
		return err
	}

	timeStampedLogFile := &TimeStampedWriter{underlying: n.LogFileMap[n.serverCmd.Process.Pid]}
	stdoutLogWriter := &LogWriter{logger: n.logger, level: zap.InfoLevel}
	stderrLogWriter := &LogWriter{logger: n.logger, level: zap.ErrorLevel}

	n.serverCmd.Stdout = io.MultiWriter(timeStampedLogFile, stdoutLogWriter)
	n.serverCmd.Stderr = io.MultiWriter(timeStampedLogFile, stderrLogWriter)

	go n.rotateLogs(n.serverCmd.Process.Pid)

	return nil
}

func (n *Nodejs) createLogFile(pid int) error {
	parentFolder := filepath.Base(filepath.Dir(n.File))
	logFileName := fmt.Sprintf("%s_server_%d_%s.log", parentFolder, n.Port, time.Now().Format("20060102"))
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}

	n.LogFileMap[pid] = logFile
	return nil
}

func (n *Nodejs) rotateLogs(pid int) {
	var logFileCount int
	for {
		// Sleep for the log rotation duration
		time.Sleep(n.LogRotationDuration)

		// Close the current log file
		if logFile, ok := n.LogFileMap[pid]; ok {
			logFile.Close()
		}

		// Create a new log file with a name based on the current timestamp
		if err := n.createLogFile(pid); err != nil {
			n.logger.Error("Failed to create new log file during rotation", zap.Int("pid", pid), zap.Error(err))
			continue
		}

		// Delete old log files
		logFileCount++
		if logFileCount > 24 {
			if err := n.deleteOldLogFiles(); err != nil {
				n.logger.Error("Failed to delete old log files", zap.Error(err))
			}
			logFileCount = 1
		}

		// Update the serverCmd stdout and stderr to the new log file
		n.serverMutex.Lock()
		if n.serverCmd != nil && n.serverCmd.Process != nil && n.serverCmd.Process.Pid == pid {
			timeStampedLogFile := &TimeStampedWriter{underlying: n.LogFileMap[pid]}
			stdoutLogWriter := &LogWriter{logger: n.logger, level: zap.InfoLevel}
			stderrLogWriter := &LogWriter{logger: n.logger, level: zap.ErrorLevel}

			n.serverCmd.Stdout = io.MultiWriter(timeStampedLogFile, stdoutLogWriter)
			n.serverCmd.Stderr = io.MultiWriter(timeStampedLogFile, stderrLogWriter)
		}
		n.serverMutex.Unlock()
	}
}

func (n *Nodejs) deleteOldLogFiles() error {
	files, err := filepath.Glob(fmt.Sprintf("%s_server_%d_*.log", filepath.Base(filepath.Dir(n.File)), n.Port))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for i := 0; i < len(files)-24; i++ {
		err := os.Remove(files[i])
		if err != nil {
			return err
		}
	}
	return nil
}

func (n *Nodejs) stopServer(pid int, lockAcquired bool) {
	n.logger.Debug("Stopping server", zap.Int("pid", pid))
	if !lockAcquired {
		n.serverMutex.Lock()
		defer n.serverMutex.Unlock()
	}

	// Find the process with the specified process ID
	process, err := os.FindProcess(pid)
	if err == nil {
		n.logger.Debug("Found process", zap.Int("pid", process.Pid))
		// First, try to send an os.Interrupt signal
		var signal os.Signal
		if runtime.GOOS == "windows" {
			signal = os.Kill
		} else {
			signal = os.Interrupt
		}
		err := process.Signal(signal)
		if err != nil {
			n.logger.Error("Failed to send interrupt signal", zap.Error(err))
			// If os.Interrupt fails, try to send an os.Kill signal
			err = process.Signal(os.Kill)
			if err != nil {
				n.logger.Error("Failed to send kill signal", zap.Error(err))
			}
		}
	} else {
		n.logger.Error("Failed to find process", zap.Error(err))
	}

	// Wait for the serverCmd process to exit with a timeout
	if n.serverCmd != nil && n.serverCmd.Process != nil && n.serverCmd.Process.Pid == pid {
		done := make(chan struct{})
		go func() {
			_, err := n.serverCmd.Process.Wait()
			if err != nil {
				n.logger.Error("Error waiting for process", zap.Error(err))
			}
			close(done)
		}()
		select {
		case <-time.After(5 * time.Second): // Adjust timeout duration as needed
			n.logger.Error("Waiting for server to stop timed out")
			if err := process.Signal(os.Kill); err != nil {
				n.logger.Error("Failed to kill the process", zap.Error(err))
			}
		case <-done:
			n.logger.Debug("Server stopped", zap.Int("pid", pid))
			n.serverStopped = true
		}
	} else {
		n.logger.Debug("Server not found or mismatching PIDs", zap.Int("serverCmd pid", n.serverCmd.Process.Pid), zap.Int("pid", pid))
	}

	// Close the log file for the stopped process
	if logFile, ok := n.LogFileMap[pid]; ok {
		logFile.Close()
		delete(n.LogFileMap, pid)
	}

	n.logger.Debug("Server stopped", zap.Int("pid", pid))
}

func (n *Nodejs) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	n.logger.Debug("Handling request")

	if n.serverCmd == nil {
		n.logger.Debug("n.serverCmd is nil")
	} else if n.serverCmd.ProcessState == nil {
		n.logger.Debug("n.serverCmd.ProcessState is nil")
	} else {
		n.logger.Debug("n.serverCmd state", zap.String("state", n.serverCmd.ProcessState.String()))
	}

	n.serverMutex.Lock()

	if n.serverCmd == nil || n.serverStopped || (n.serverCmd.ProcessState != nil && n.serverCmd.ProcessState.Exited()) {
		n.logger.Debug("Starting new server")
		err := n.startServer()
		if err != nil {
			return fmt.Errorf("failed to start node.js server: %v", err)
		}

		select {
		case <-time.After(5 * time.Second): // Adjust timeout duration as needed
			n.logger.Debug("Waiting for n.serverReady timed out")
			return fmt.Errorf("waiting for server to be ready timed out")
		case <-func() chan struct{} {
			done := make(chan struct{})
			go func() {
				n.serverReady.Wait()
				close(done)
			}()
			return done
		}():
			n.logger.Debug("Waiting done for n.serverReady")
		}
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

func (n *Nodejs) monitorIdleTime(pid int) {
	for {
		time.Sleep(60 * time.Second)
		n.logger.Debug("Checking if server is idle", zap.Int("pid", pid))
		n.serverMutex.Lock()
		if time.Since(n.lastActive) > n.timeout {
			n.stopServer(pid, true)
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
