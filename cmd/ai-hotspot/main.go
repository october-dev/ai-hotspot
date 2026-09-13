// Command ai-hotspot runs a LAN, OpenAI-compatible model-router HTTP server.
//
// The operator holds the real provider API keys; anyone on the same LAN uses
// models through those keys with no accounts — just a base URL and a client key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/harshsaver/ai-hotspot/internal/config"
	"github.com/harshsaver/ai-hotspot/internal/server"
	"github.com/harshsaver/ai-hotspot/internal/store"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	portOverride := flag.Int("port", 0, "override the port from config (0 = use config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if *portOverride != 0 {
		cfg.Port = *portOverride
	}

	st, err := store.New(cfg.DataFile)
	if err != nil {
		log.Fatalf("store error: %v", err)
	}

	srv := server.New(cfg, st)
	addr := "0.0.0.0:" + strconv.Itoa(cfg.Port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	printBanner(cfg, *configPath)

	errc := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errc:
		log.Fatalf("server error: %v", err)
	case sig := <-stop:
		log.Printf("received %s, shutting down...", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("stopped")
}

func printBanner(cfg *config.Config, configPath string) {
	fmt.Println("======================================================")
	fmt.Println("  AI Hotspot — LAN OpenAI-compatible model router")
	fmt.Println("======================================================")

	ips := lanIPv4s()
	if len(ips) == 0 {
		fmt.Printf("AI Hotspot on http://0.0.0.0:%d  (dashboard: http://0.0.0.0:%d/)\n", cfg.Port, cfg.Port)
	}
	for _, ip := range ips {
		fmt.Printf("AI Hotspot on http://%s:%d  (dashboard: http://%s:%d/)\n", ip, cfg.Port, ip, cfg.Port)
	}

	fmt.Println("------------------------------------------------------")
	fmt.Printf("Listening on:    0.0.0.0:%d\n", cfg.Port)
	fmt.Printf("Admin token:     defined in %s (\"adminToken\")\n", configPath)
	fmt.Printf("State file:      %s\n", cfg.DataFile)
	fmt.Printf("Default upstream: %s\n", cfg.DefaultUpstream)
	fmt.Print("Upstreams:       ")
	for i, u := range cfg.Upstreams {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Print(u.Name)
	}
	fmt.Println()
	fmt.Println("------------------------------------------------------")
	fmt.Println("Open the dashboard, paste the admin token, create client keys,")
	fmt.Println("and share a LAN URL above + a client key with people on your WiFi.")
	fmt.Println("Press Ctrl+C to stop.")
	fmt.Println()
}

// lanIPv4s returns all non-loopback IPv4 addresses on the machine.
func lanIPv4s() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			out = append(out, ip4.String())
		}
	}
	return out
}
