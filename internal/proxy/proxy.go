package proxy

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"dpi/internal/conn"
	prettylog "dpi/internal/pkg/log"
)

type ProxyServer struct {
	host string
	port string

	blacklist     []string
	blacklistFile string
	noBlacklist   bool
	logAccessFile string
	logErrorFile  string
	quiet         bool
	verbose       bool

	totalConnections   uint64
	allowedConnections uint64
	blockedConnections uint64

	trafficIN  uint64
	trafficOUT uint64

	lastTrafficIN  uint64
	lastTrafficOUT uint64

	speedIN  uint64
	speedOUT uint64

	lastTime time.Time

	ActiveConnections map[string]conn.ConnectionInfo
	ConnMutex         sync.Mutex

	log *slog.Logger
}

func NewProxy(log *slog.Logger) *ProxyServer {
	const defaultHost = "127.0.0.1"
	const defaultPort = ":8881"

	return &ProxyServer{
		host:              defaultHost,
		port:              defaultPort,
		ActiveConnections: make(map[string]conn.ConnectionInfo),
		ConnMutex:         sync.Mutex{},
		blacklist:         []string{},
		lastTime:          time.Now(),
		log:               log,
	}
}

func (p *ProxyServer) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Get client IP and port
	clientAddr := clientConn.RemoteAddr().String()
	clientIP, clientPortStr, err := net.SplitHostPort(clientAddr)
	if err != nil {
		p.log.Warn("Failed to parse client address:", slog.Any("error", err.Error()))
		return
	}

	// Read initial HTTP data
	reader := bufio.NewReader(clientConn)
	httpData := make([]byte, 1500)
	n, err := reader.Read(httpData)

	if err != nil || n == 0 {
		p.log.Warn("Failed to read HTTP data:", slog.Any("error", err.Error()))
		return
	}

	httpData = httpData[:n]

	// Parse headers
	headers := bytes.Split(httpData, []byte("\r\n"))
	if len(headers) < 1 {
		p.log.Warn("Invalid HTTP headers")
		return
	}

	// Parse first line
	firstLine := bytes.Split(headers[0], []byte(" "))
	if len(firstLine) < 2 {
		p.log.Warn("Invalid HTTP request line")
		return
	}

	method := firstLine[0]
	url := firstLine[1]

	var host string
	var port int

	switch string(method) {
	case http.MethodConnect:
		// Handle CONNECT request
		hostPort := bytes.Split(url, []byte(":"))

		host = string(hostPort[0])
		port = 443

		if len(hostPort) > 1 {
			port, err = strconv.Atoi(string(hostPort[1]))
			if err != nil {
				p.log.Warn("Invalid port in CONNECT URL", slog.Any("error", err))
				return
			}
		}
	default:
		// Handle other HTTP methods
		var hostHeader []byte
		for _, header := range headers {
			if bytes.HasPrefix(header, []byte("Host: ")) {
				hostHeader = header[6:]
				break
			}
		}

		if len(hostHeader) == 0 {
			p.log.Warn("Missing Host header")
			return
		}

		hostPort := bytes.Split(hostHeader, []byte(":"))
		host = string(hostPort[0])
		port = 80

		if len(hostPort) > 1 {
			port, err = strconv.Atoi(string(hostPort[1]))
			if err != nil {
				p.log.Warn("Invalid port in Host header", slog.Any("error", err))
				return
			}
		}
	}

	// Check blacklist
	if !p.noBlacklist && slices.Contains(p.blacklist, host) {
		p.blockedConnections++
		p.log.Warn("Connection to blacklisted host", slog.String("host", host))
		return
	}

	// Store connection info
	connKey := net.JoinHostPort(clientIP, clientPortStr)

	connInfo := conn.ConnectionInfo{
		SrcIP:      clientIP,
		DstDomain:  host,
		Method:     string(method),
		StartTime:  time.Now(),
		TrafficIN:  0,
		TrafficOUT: 0,
	}

	p.ConnMutex.Lock()
	p.ActiveConnections[connKey] = connInfo
	p.ConnMutex.Unlock()

	// Connect to remote server
	remoteConn, err := net.Dial(
		"tcp",
		net.JoinHostPort(host, strconv.Itoa(port)),
	)
	if err != nil {
		p.log.Warn("Failed to connect to remote:", slog.Any("error", err.Error()))
		return
	}

	defer remoteConn.Close()

	switch string(method) {

	case http.MethodConnect:
		// Send 200 Connection Established for CONNECT
		_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		if err != nil {
			p.log.Warn("Failed to write response", slog.Any("error", err))
			return
		}
		// Start fragmenting data
		reader := bufio.NewReader(clientConn)
		p.fragmentData(reader, remoteConn)
	default:
		// Forward initial HTTP data for non-CONNECT
		_, err = remoteConn.Write(httpData)
		if err != nil {
			p.log.Warn("Failed to write to remote", slog.Any("error", err))
			return
		}
		p.ConnMutex.Lock()
		p.allowedConnections++
		p.ConnMutex.Unlock()
	}

	p.ConnMutex.Lock()
	p.totalConnections++
	p.ConnMutex.Unlock()

	// Pipe data in both directions
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		p.pipe(clientConn, remoteConn, "out", connKey)
	}()

	go func() {
		defer wg.Done()
		p.pipe(remoteConn, clientConn, "in", connKey)
	}()

	wg.Wait()
}

func Run() error {
	var (
		host        = flag.String("host", "127.0.0.1", "Proxy host")
		port        = flag.String("port", "8881", "Proxy port")
		blacklist   = flag.String("blacklist", "blacklist.txt", "Path to blacklist file")
		logAccess   = flag.String("log_access", "", "Path to the access control log")
		logError    = flag.String("log_error", "", "Path to log file for errors")
		noBlacklist = flag.Bool("no_blacklist", false, "Use fragmentation for all domains")
		quiet       = flag.Bool("quiet", false, "Remove UI output")
		verbose     = flag.Bool("verbose", false, "Show more info (only for devs)")
	)
	flag.Parse()

	logger := setupPrettySlog()

	proxy := NewProxy(logger).
		WithHost(*host).
		WithPort(*port).
		WithBlacklist(*blacklist).
		WithLogAccess(*logAccess).
		WithLogError(*logError).
		WithNoBlacklist(*noBlacklist).
		WithQuiet(*quiet).
		WithVerbose(*verbose)

	if err := proxy.loadBlacklist(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		<-sigChan
		<-sigChan // Require two Ctrl+C to shutdown
		cancel()
	}()

	return proxy.Start(ctx)
}

func setupPrettySlog() *slog.Logger {
	opts := prettylog.PrettyHandlerOptions{
		SlogOpts: &slog.HandlerOptions{
			Level: slog.LevelDebug,
		},
	}

	handler := opts.NewPrettyHandler(os.Stdout)

	return slog.New(handler)
}

// pipe copies data between two connections
func (p *ProxyServer) pipe(src, dst net.Conn, direction, connKey string) {
	defer func() {
		if direction == "out" {
			dst.Close()
		}
		p.ConnMutex.Lock()
		if connInfo, exists := p.ActiveConnections[connKey]; exists {
			p.log.Info(connInfo.String())
			delete(p.ActiveConnections, connKey)
		}
		p.ConnMutex.Unlock()
	}()

	for {
		data := make([]byte, 1500)
		n, err := src.Read(data)
		if err != nil {
			if err != io.EOF {
				p.log.Warn(
					"Pipe error",
					slog.String("direction", direction),
					slog.Any("error", err),
				)
			}
			return
		}
		data = data[:n]

		p.ConnMutex.Lock()
		if connInfo, exists := p.ActiveConnections[connKey]; exists {
			if direction == "in" {
				connInfo.TrafficIN += uint64(n)
				p.trafficIN += uint64(n)
			} else {
				connInfo.TrafficOUT += uint64(n)
				p.trafficOUT += uint64(n)
			}
			p.ActiveConnections[connKey] = connInfo
		}
		p.ConnMutex.Unlock()

		_, err = dst.Write(data)
		if err != nil {
			p.log.Warn(
				"Pipe write error",
				slog.String("direction", direction),
				slog.Any("error", err),
			)
			return
		}
	}
}

// fragmentData is a placeholder for the fragment_data function
func (p *ProxyServer) fragmentData(src *bufio.Reader, dst net.Conn) {
	head := make([]byte, 5)
	_, err := io.ReadFull(src, head)
	if err != nil {
		p.log.Warn("Failed to read head in fragmentData", slog.Any("error", err))
		return
	}

	data := make([]byte, 2048)
	n, err := src.Read(data)
	if err != nil && err != io.EOF {
		p.log.Warn("Failed to read data in fragmentData", slog.Any("error", err))
		return
	}
	data = data[:n]

	if p.noBlacklist || !slices.ContainsFunc(p.blacklist, func(site string) bool {
		return strings.Contains(string(data), site)
	}) {
		p.ConnMutex.Lock()
		p.allowedConnections++
		p.ConnMutex.Unlock()
		_, err = dst.Write(append(head, data...))
		if err != nil {
			p.log.Warn("Failed to write in fragmentData", slog.Any("error", err))
		}
		return
	}

	p.ConnMutex.Lock()
	p.blockedConnections++
	p.ConnMutex.Unlock()

	var parts [][]byte
	hostEnd := bytes.IndexByte(data, 0)
	if hostEnd != -1 {
		parts = append(parts, append(
			[]byte{0x16, 0x03, 0x04},
			[]byte{byte((hostEnd + 1) >> 8), byte(hostEnd + 1)}...,
		))
		parts = append(parts, data[:hostEnd+1])
		data = data[hostEnd+1:]
	}

	for len(data) > 0 {
		chunkLen := rand.Intn(len(data)) + 1
		parts = append(parts, append(
			[]byte{0x16, 0x03, 0x04},
			[]byte{byte(chunkLen >> 8), byte(chunkLen)}...,
		))
		parts = append(parts, data[:chunkLen])
		data = data[chunkLen:]
	}

	_, err = dst.Write(bytes.Join(parts, nil))
	if err != nil {
		p.log.Warn("Failed to write fragmented data", slog.Any("error", err))
	}
}

func (p *ProxyServer) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(p.host, p.port))
	if err != nil {
		p.log.Error("Failed to start server", slog.Any("error", err))
		return err
	}

	p.log.Info("Proxy server started", slog.String("address", net.JoinHostPort(p.host, p.port)))

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				p.log.Info("Shutting down proxy...")
				return nil
			}
			p.log.Warn("Failed to accept connection", slog.Any("error", err))
			continue
		}
		go p.handleConnection(conn)
	}
}

func (p *ProxyServer) WithHost(host string) *ProxyServer {
	p.host = host
	return p
}

func (p *ProxyServer) WithPort(port string) *ProxyServer {
	p.port = port
	return p
}

func (p *ProxyServer) WithBlacklist(file string) *ProxyServer {
	p.blacklistFile = file
	return p
}

func (p *ProxyServer) WithLogAccess(file string) *ProxyServer {
	p.logAccessFile = file
	return p
}

func (p *ProxyServer) WithLogError(file string) *ProxyServer {
	p.logErrorFile = file
	return p
}

func (p *ProxyServer) WithNoBlacklist(noBlacklist bool) *ProxyServer {
	p.noBlacklist = noBlacklist
	return p
}

func (p *ProxyServer) WithQuiet(quiet bool) *ProxyServer {
	p.quiet = quiet
	return p
}

func (p *ProxyServer) WithVerbose(verbose bool) *ProxyServer {
	p.verbose = verbose
	return p
}

func (p *ProxyServer) loadBlacklist() error {
	if p.blacklistFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.blacklistFile)
	if err != nil {
		p.log.Error("File not found", slog.String("file", p.blacklistFile))
		return fmt.Errorf("failed to read blacklist file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	p.blacklist = make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			p.blacklist = append(p.blacklist, line)
		}
	}
	return nil
}
