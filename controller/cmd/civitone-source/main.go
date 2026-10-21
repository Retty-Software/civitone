// Command civitone-source is Civitone Source: it captures audio on this
// computer (its own sound, an input, a test chime) and sends it to a
// Civitone server. Configure it at http://127.0.0.1:8091.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Retty-Software/civitone/controller/internal/srcapp"
)

func main() {
	var (
		listen  = flag.String("listen", "127.0.0.1:8091", "setup page address (keep it on 127.0.0.1)")
		cfgPath = flag.String("config", srcapp.DefaultPath(), "settings file")
		open    = flag.Bool("open", false, "open the setup page in the browser (always on first run)")
		version = flag.Bool("version", false, "print the version and exit")
		logPath = flag.String("log", defaultLogPath(), "log file (empty = stderr)")
	)
	flag.Parse()
	if *version {
		fmt.Println(srcapp.Version)
		return
	}
	if *logPath != "" {
		if f, err := openLog(*logPath); err == nil {
			log.SetOutput(f)
			defer f.Close()
		}
	}

	store, err := srcapp.Open(*cfgPath)
	if err != nil {
		log.Fatalf("settings %s: %v", *cfgPath, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		// Most likely already running (started at login): just show it.
		log.Printf("listen %s: %v; opening the running copy's page", *listen, err)
		openBrowser("http://" + *listen)
		return
	}
	app := srcapp.New(ctx, store)
	srv := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()

	url := "http://" + ln.Addr().String()
	log.Printf("Civitone Source %s: %s (settings %s)", srcapp.Version, url, *cfgPath)
	if *open || store.Get().Server == "" {
		openBrowser(url)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("serve: %v", err)
			stop()
		}
	}()
	runUI(ctx, url, app, stop)
	log.Printf("Civitone Source stopping")
}

// defaultLogPath is a file on Windows, where the app has no console
// (built with -H windowsgui), and stderr elsewhere.
func defaultLogPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Civitone", "civitone-source.log")
}

// openLog appends to path, starting over once it passes 5 MB.
func openLog(path string) (*os.File, error) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o644)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open browser: %v (open %s yourself)", err, url)
	}
}
