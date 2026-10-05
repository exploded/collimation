// Command collimation is an image-based collimation station for an imaging
// Newtonian. Run it with no arguments (start.bat) to start the web UI.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/nina"
	"github.com/exploded/collimation/internal/station"
	"github.com/exploded/collimation/internal/web"
)

// defaultPort is the web UI port. COLLIM_PORT overrides it.
const defaultPort = "8780"

func main() {
	if len(os.Args) > 1 {
		var err error
		switch os.Args[1] {
		case "analyse":
			err = runAnalyse(os.Args[2:])
		case "stub-nina":
			err = runStub(os.Args[2:])
		default:
			err = fmt.Errorf("unknown command %q (run with no arguments to start the web UI)", os.Args[1])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(); err != nil {
		log.Println("error:", err)
		if runtime.GOOS == "windows" {
			fmt.Println("\nPress Enter to close.")
			fmt.Scanln()
		}
		os.Exit(1)
	}
}

func serve() error {
	port := envOr("COLLIM_PORT", defaultPort)
	dbPath := envOr("COLLIM_DB", filepath.Join(appDir(), "collimation.db"))
	conn, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dbPath, err)
	}
	defer conn.Close()

	st := station.New(conn, analysis.DefaultConfig())
	srv, err := web.New(st)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return fmt.Errorf("port %s is in use. Is the collimation app already running? (%w)", port, err)
	}
	local := "http://localhost:" + port
	fmt.Println("Collimation station")
	fmt.Println("  This PC:        ", local)
	for _, u := range lanURLs(port) {
		fmt.Println("  Laptop or phone:", u)
	}
	fmt.Println("  Database:       ", dbPath)
	fmt.Println("Close this window to stop.")
	if os.Getenv("COLLIM_NOBROWSER") == "" {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(local)
		}()
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runStub runs a fake N.I.N.A. that serves saved frames, for testing
// without the telescope.
func runStub(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: collimation stub-nina <folder of FITS frames> [port]")
	}
	port := "1888"
	if len(args) > 1 {
		port = args[1]
	}
	stub, err := nina.NewStub(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("Stub N.I.N.A. on :%s serving %s\n", port, args[0])
	return http.ListenAndServe(":"+port, stub)
}

// appDir is the folder holding the executable, or the working directory
// under `go run`.
func appDir() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		if !strings.Contains(strings.ToLower(dir), "go-build") {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func lanURLs(port string) []string {
	var out []string
	if h, err := os.Hostname(); err == nil {
		out = append(out, fmt.Sprintf("http://%s:%s", strings.ToLower(h), port))
	}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, fmt.Sprintf("http://%s:%s", ipn.IP, port))
			}
		}
	}
	return out
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
	_ = cmd.Start()
}
