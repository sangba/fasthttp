package fasthttp

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

const (
	envListenFds = "LISTEN_FDS"
)

var (
	app        *App
	pidInherit = os.Getenv(envListenFds) != ""
	ppid       = os.Getppid()
)

func init() {
	app = &App{}
}

// AppServer ...
type AppServer struct {
	Addr    string
	Handler RequestHandler
}

// Grace ...
func Grace(servers ...AppServer) {
	app.errors = make(chan error, 1+(len(servers)*2))
	app.Serve(servers)
}

// App ...
type App struct {
	listeners []net.Listener
	Servers   []*Server
	errors    chan error
	wg        sync.WaitGroup
	isInherit bool
	isFork    bool
}

// AppListenAndServe ...
func AppListenAndServe(addr string, handler RequestHandler) error {
	s := &Server{
		Handler: handler,
	}

	var ln net.Listener
	var err error

	ln, err = getListener(addr)
	if err != nil {
		log.Println("getListener err: ", err)
		return err
	}

	app.Servers = append(app.Servers, s)

	if s.TCPKeepalive {
		if tcpln, ok := ln.(*net.TCPListener); ok {
			app.listeners = append(app.listeners, tcpln)

			return s.Serve(tcpKeepaliveListener{
				TCPListener:     tcpln,
				keepalivePeriod: s.TCPKeepalivePeriod,
			})
		}
	} else {
		app.listeners = append(app.listeners, ln)
	}

	return s.Serve(ln)
}

func getListener(addr string) (net.Listener, error) {

	if app.isInherit {
		addrStr := os.Getenv(envListenFds)
		addrs := strings.Split(addrStr, ",")

		if strings.Contains(addr, ":") {
			arr := strings.Split(addr, ":")
			addr = arr[len(arr)-1]
		}

		offset := 0

		for i, v := range addrs {
			if addr == strings.Split(v, ":")[1] {
				offset = i
				break
			}
		}

		f := os.NewFile(uintptr(3+offset), "")
		return net.FileListener(f)
	}

	return net.Listen("tcp4", addr)
}

// Serve ...
func (a *App) Serve(servers []AppServer) {
	app.wg.Add(len(servers))

	// Start serving
	for _, server := range servers {
		go func(s AppServer) {
			if err := AppListenAndServe(s.Addr, s.Handler); err != nil {
				a.errors <- err
			}
		}(server)
	}

	go a.signalHandler()

	waitdone := make(chan struct{})

	go func() {
		defer func() {
			close(waitdone)
		}()
		app.wg.Wait()
	}()

	if pidInherit && ppid != 1 {
		a.isInherit = true

		if err := syscall.Kill(ppid, syscall.SIGTERM); err != nil {
			a.errors <- err
		}
	}

	select {
	case err := <-app.errors:
		log.Println(os.Getpid(), "errors: ", err)
		if err != nil {
			panic(err)
		}
	case <-waitdone:
		log.Println(os.Getpid(), "waitdone ...")
		break
	}
}

func (a *App) signalHandler() {
	defer func() {
		log.Println(os.Getpid(), "signalHandler exit.")
	}()

	ch := make(chan os.Signal, 10)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)

	pid := syscall.Getpid()

	for {
		sig := <-ch

		switch sig {
		case syscall.SIGTERM:
			log.Println(pid, "Received SIGTERM.")
			a.shutdown()
			return
		case syscall.SIGINT:
			log.Println(pid, "Received SIGINT.")
			a.shutdown()
			return
		case syscall.SIGUSR2:
			log.Println(pid, "Received SIGUSR2. forking.")
			if err := a.fork(); err != nil {
				log.Println("Fork err:", err)
			}
		}
	}
}

func (a *App) shutdown() {
	defer func() {
		log.Println(os.Getpid(), "shutdown exit.")
	}()

	// 方案1
	for _, s := range a.Servers {
		go func(a *App, s *Server) {
			defer a.wg.Done()
			s.Shutdown()
		}(a, s)
	}

	// 方案2
	// ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// defer cancel()

	// for _, s := range a.Servers {
	// 	go func(a *App, s *Server) {
	// 		defer a.wg.Done()
	// 		s.Shutdown()
	// 	}(a, s)
	// }

	// select {
	// case <-ctx.Done(): // 5s退出超时
	// 	log.Println("shutdown timeout")
	// 	a.wg.Done()
	// }
}

func (a *App) fork() error {
	path := os.Args[0]

	var args []string = os.Args[1:]
	var files []*os.File
	var env []string

	// environ
	var addrs []string
	for _, s := range a.listeners {
		addrs = append(addrs, s.Addr().String())
	}

	env = append(env, fmt.Sprintf("%s=%s", envListenFds, strings.Join(addrs, ",")))

	// listeners
	for _, ln := range a.listeners {
		f, _ := ln.(*net.TCPListener).File()
		files = append(files, f)
	}

	cmd := exec.Command(path, args...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.ExtraFiles = files
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		log.Fatalf("Restart: Failed to launch, error: %v", err)
	}

	a.isFork = true

	return nil
}

// IsFork ...
func IsFork() bool {
	return app.isFork
}
